package amper

import (
	"fmt"
	"sort"
	"sync"
)

type Attachment struct {
	pt      *PolicyTemplate
	account *Account
	vars    map[string]string
}

func (a Attachment) String() string {
	return a.pt.Key
}

type Container struct {
	sync.RWMutex

	amper *Kernel

	ID string

	attachments []*Attachment
}

func (c *Container) AddPolicyTemplate(pt *PolicyTemplate) error {
	c.amper.Lock()
	defer c.amper.Unlock()

	if _, ok := c.amper.policyTemplates[pt.Key]; ok {
		return fmt.Errorf("policy template '%s' already exists", pt.Key)
	}

	if pt.container != nil || pt.amper != nil {
		return fmt.Errorf("policy '%s' is in unknown state", pt.Key)
	}

	pt.amper = c.amper
	pt.container = c

	c.amper.policyTemplates[pt.Key] = pt

	return nil
}

func (c *Container) AddAttachment(policyTemplateID string, accountName string, vars map[string]string) (*Attachment, error) {
	c.amper.RLock()
	defer c.amper.RUnlock()

	c.Lock()
	defer c.Unlock()

	pt, ok := c.amper.policyTemplates[policyTemplateID]

	if !ok {
		return nil, fmt.Errorf("cannot add attachment, unknown policy template '%s' in container '%s'", policyTemplateID, c.ID)
	}

	account, ok := c.amper.accounts[accountName]

	if !ok {
		return nil, fmt.Errorf("cannot add attachment, unknown account '%s' in container '%s'", accountName, c.ID)
	}

	for _, varName := range pt.Vars {
		if _, ok := vars[varName]; !ok {
			return nil, fmt.Errorf("cannot add attachment, variable '%s' is not set in container '%s'", varName, c.ID)
		}
	}

	attachment := &Attachment{
		pt:      pt,
		account: account,
		vars:    vars,
	}

	c.attachments = append(c.attachments, attachment)

	return attachment, nil
}

func (c *Container) Policy() (_ *Policy, err error, missing []*Attachment) {
	c.amper.RLock()
	defer c.amper.RUnlock()

	c.RLock()
	defer c.RUnlock()

	p := &Policy{
		amper: c.amper,
	}

	accountPolicies := make(map[string][]*IAMPolicyDoc)
	accountRolePolicies := make(map[string][]*IAMPolicyDoc)
	serviceRolePolicies := make(map[string]map[string]*ServiceRolePolicy)
	scopeMap := make(map[string]map[string]bool)

	for _, a := range c.attachments {
		if serviceRolePolicies[a.account.Name] == nil {
			serviceRolePolicies[a.account.Name] = make(map[string]*ServiceRolePolicy)
		}

		pd, err := a.pt.renderTemplate(c, a.account, a.vars)

		if err != nil {
			return nil, err, nil
		}

		if scopeMap[a.account.Name] == nil {
			scopeMap[a.account.Name] = make(map[string]bool)
		}

		if pd == nil {
			// Policy not found
			fmt.Printf("[WARN] Policy template '%s' not found\n", a.pt.Key)
			accountPolicies[a.account.Name] = append(accountPolicies[a.account.Name], &IAMPolicyDoc{})
			missing = append(missing, a)
			continue
		}

		if pd.Version != "" && pd.Version != IAMPolicyVersion {
			return nil, fmt.Errorf("Unsupported policy version '%s'", pd.Version), nil
		}

		accountPolicies[a.account.Name] = append(accountPolicies[a.account.Name], pd)

		for _, s := range a.pt.Scope {
			scopeMap[a.account.Name][s] = true
		}

		if a.pt.ServiceRole != nil {
			srp := &ServiceRolePolicy{}

			srp.Policy, err = a.pt.renderServiceRole(c, a.account, a.vars)

			if err != nil {
				return nil, err, nil
			}

			srp.AssumeRolePolicy, err = a.pt.renderServiceAssumeRole(c, a.account, a.vars)

			if err != nil {
				return nil, err, nil
			}

			serviceRolePolicies[a.account.Name][a.pt.ServiceRole.Name] = srp
		}
	}

	allowAll := &IAMPolicyStatement{
		Sid:       "AllowAll",
		Effect:    "Allow",
		Actions:   []string{"*"},
		Resources: []string{"*"},
	}

	for account, po := range scopeMap {
		var denyUnknown *IAMPolicyStatement

		if len(po) == 0 {
			// Nothing will be allowed!
			denyUnknown = &IAMPolicyStatement{
				Sid:       "DenyAll",
				Effect:    "Deny",
				Actions:   []string{"*"},
				Resources: []string{"*"},
			}
		} else {
			scopes := make([]string, 0, len(po))

			for k := range po {
				scopes = append(scopes, k)
			}

			sort.Sort(sort.StringSlice(scopes))

			denyUnknown = &IAMPolicyStatement{
				Sid:        "DenyUnknownServices",
				Effect:     "Deny",
				NotActions: scopes,
				Resources:  []string{"*"},
			}
		}

		accountPolicies[account] = append(accountPolicies[account], &IAMPolicyDoc{
			Statements: []*IAMPolicyStatement{denyUnknown},
		})

		accountRolePolicies[account] = accountPolicies[account]

		if len(po) > 0 {
			accountPolicies[account] = append(accountPolicies[account], &IAMPolicyDoc{
				Statements: []*IAMPolicyStatement{allowAll},
			})
		}
	}

	p.AccountPolicies = accountPolicies
	p.AccountRolePolicies = accountRolePolicies
	p.ServiceRolePolicies = serviceRolePolicies

	if err = p.compress(); err != nil {
		return
	}

	return p, nil, missing
}
