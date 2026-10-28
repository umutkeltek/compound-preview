package runtimebinding

import (
	"fmt"

	"compound/internal/reviewartifact"
)

func (p RepairPlan) ReviewOperatorCard() (reviewartifact.OperatorCard, error) {
	return OperatorCardForRepairPlan(p)
}

func OperatorCardForRepairPlan(p RepairPlan) (reviewartifact.OperatorCard, error) {
	service, err := reviewartifact.RequireText("service_id", p.ServiceID)
	if err != nil {
		return reviewartifact.OperatorCard{}, err
	}
	count := p.FailedCount
	if count == 0 {
		count = len(p.FailedChecks)
	}
	return reviewartifact.NewOperatorCard(
		fmt.Sprintf("Runtime repair found %d failed checks for %s. Review owner repair?", count, reviewartifact.ShortLabel(service, 42)),
		service,
		"runtime-repair|"+p.ServiceID,
		"runtime bindings change only through reviewed writeback",
	)
}
