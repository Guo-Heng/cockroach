// Copyright 2021 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package opgen

import (
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/rel"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scop"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scpb"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/screl"
	"github.com/cockroachdb/errors"
)

// target represents the operation generation rules for a given Target.
type target struct {
	e           scpb.Element
	status      scpb.Status
	transitions []transition
	iterateFunc func(*rel.Database, func(*screl.Node) error) error
}

// transition represents a transition from one status to the next towards a
// Target.
type transition struct {
	from, to   scpb.Status
	revertible bool
	ops        opsFunc
	minPhase   scop.Phase
}

func makeTarget(e scpb.Element, spec targetSpec) (t target, err error) {
	defer func() {
		err = errors.Wrapf(err, "target %s", spec.to)
	}()
	t = target{
		e:      e,
		status: spec.to,
	}
	t.transitions, err = makeTransitions(e, spec)
	if err != nil {
		return t, err
	}

	// Make iterator function for traversing graph nodes with this target.
	var element, target, node, targetStatus rel.Var = "element", "target", "node", "target-status"
	q, err := rel.NewQuery(screl.Schema,
		element.Type(e),
		element.AttrEqVar(screl.DescID, "descID"), // this is to allow the index on elements to work
		targetStatus.Eq(spec.to),
		screl.JoinTargetNode(element, target, node),
		target.AttrEqVar(screl.TargetStatus, targetStatus),
	)
	if err != nil {
		return t, errors.Wrap(err, "failed to construct query")
	}
	t.iterateFunc = func(database *rel.Database, f func(*screl.Node) error) error {
		return q.Iterate(database, func(r rel.Result) error {
			return f(r.Var(node).(*screl.Node))
		})
	}

	return t, nil
}

func makeTransitions(e scpb.Element, spec targetSpec) (ret []transition, err error) {
	tbs := makeTransitionBuildState(spec.from)
	for _, s := range spec.transitionSpecs {
		var t transition
		if s.from == scpb.Status_UNKNOWN {
			t.from = tbs.from
			t.to = s.to
			if err := tbs.withTransition(s); err != nil {
				return nil, errors.Wrapf(err, "invalid transition %s -> %s", t.from, t.to)
			}
			if len(s.emitFns) > 0 {
				t.ops, err = makeOpsFunc(e, s.emitFns)
				if err != nil {
					return nil, errors.Wrapf(err, "making ops func for transition %s -> %s", t.from, t.to)
				}
			}
		} else {
			t.from = s.from
			t.to = tbs.from
			if err := tbs.withEquivTransition(s); err != nil {
				return nil, errors.Wrapf(err, "invalid no-op transition %s -> %s", t.from, t.to)
			}
		}
		t.revertible = tbs.isRevertible
		t.minPhase = tbs.currentMinPhase
		ret = append(ret, t)
	}

	// Check that the final status has been reached.
	if tbs.from != spec.to {
		return nil, errors.Errorf("expected %s as the final status, instead found %s", spec.to, tbs.from)
	}

	return ret, nil
}

type transitionBuildState struct {
	from            scpb.Status
	currentMinPhase scop.Phase
	isRevertible    bool

	isEquivMapped map[scpb.Status]bool
	isTo          map[scpb.Status]bool
	isFrom        map[scpb.Status]bool
}

func makeTransitionBuildState(from scpb.Status) transitionBuildState {
	return transitionBuildState{
		from:          from,
		isRevertible:  true,
		isEquivMapped: map[scpb.Status]bool{from: true},
		isTo:          map[scpb.Status]bool{},
		isFrom:        map[scpb.Status]bool{},
	}
}

func (tbs *transitionBuildState) withTransition(s transitionSpec) error {
	// Check validity of target status.
	if s.to == scpb.Status_UNKNOWN {
		return errors.Errorf("invalid 'to' status")
	}
	if tbs.isTo[s.to] {
		return errors.Errorf("%s was featured as 'to' in a previous transition", s.to)
	} else if tbs.isEquivMapped[s.to] {
		return errors.Errorf("%s was featured as 'from' in a previous equivalence mapping", s.to)
	}

	// Check that the minimum phase is monotonically increasing.
	if s.minPhase > 0 && s.minPhase < tbs.currentMinPhase {
		return errors.Errorf("minimum phase %s is less than inherited minimum phase %s",
			s.minPhase.String(), tbs.currentMinPhase.String())
	}

	tbs.isRevertible = tbs.isRevertible && s.revertible
	if s.minPhase > tbs.currentMinPhase {
		tbs.currentMinPhase = s.minPhase
	}
	tbs.isEquivMapped[tbs.from] = true
	tbs.isTo[s.to] = true
	tbs.isFrom[tbs.from] = true
	tbs.from = s.to
	return nil
}

func (tbs *transitionBuildState) withEquivTransition(s transitionSpec) error {
	// Check validity of status pair.
	if s.to != scpb.Status_UNKNOWN {
		return errors.Errorf("invalid 'to' status %s", s.to)
	}

	// Check validity of origin status.
	if tbs.isTo[s.from] {
		return errors.Errorf("%s was featured as 'to' in a previous transition", s.from)
	} else if tbs.isEquivMapped[s.from] {
		return errors.Errorf("%s was featured as 'from' in a previous equivalence mapping", s.from)
	}

	// Check for absence of phase and revertibility constraints
	if !s.revertible {
		return errors.Errorf("must be revertible")
	}
	if s.minPhase > 0 {
		return errors.Errorf("must not set a minimum phase")
	}

	tbs.isEquivMapped[s.from] = true
	return nil
}
