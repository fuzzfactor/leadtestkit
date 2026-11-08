// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"context"
	"path"
)

// workflowName returns the name of a workflow file, or "" if it is not
// known.  Runs of dispatched workflows are named after their request,
// so their reports and checks take the name of the workflow instead.
func (p *Pipeline) workflowName(ctx context.Context, file string) string {
	if p.workflowNames == nil && p.workflowNamesStep != p.steps {
		p.workflowNamesStep = p.steps
		list, err := p.GH.Workflows(ctx)
		if err != nil {
			p.warnf("Cannot list the workflows of the test repository to name "+
				"dispatched runs; trying again in the next cycle: %v", err)
			return ""
		}
		p.workflowNames = map[string]string{}
		for _, w := range list {
			p.workflowNames[path.Base(w.Path)] = w.Name
		}
	}
	return p.workflowNames[file]
}
