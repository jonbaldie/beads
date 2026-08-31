package workapi

import (
	"github.com/jonbaldie/beads/internal/storage/domain"
	"github.com/jonbaldie/beads/internal/types"
)

func (c ListConfig) CustomStatusNames() []string {
	out := make([]string, len(c.CustomStatuses))
	for i, s := range c.CustomStatuses {
		out[i] = s.Name
	}
	return out
}

func (c ListConfig) InfraTypes() []string {
	if len(c.InfraSet) == 0 {
		return domain.DefaultInfraTypes()
	}
	out := make([]string, 0, len(c.InfraSet))
	for t := range c.InfraSet {
		out = append(out, t)
	}
	return out
}

func (c ListConfig) IsInfra(t string) bool {
	if len(c.InfraSet) == 0 {
		return domain.IsInfraType(types.IssueType(t))
	}
	return c.InfraSet[t]
}
