package store

import (
	"fmt"
	"math/rand"
	"testing"
)

// This file checks the task graph's contract as a whole, after arbitrary
// sequences of the operations the server performs on it. The invariant is the
// one the model promises:
//
//   - a container's status and counts are its children's, by the documented
//     precedence (failed > running > pending > skipped > passed), with a real
//     child counting one unit and a virtual child contributing its subtree's
//     tally;
//   - a real node's status and counts are its current attempt's run's, and the
//     run for the current attempt exists;
//   - no superseded attempt is left non-terminal: nothing would ever close it.
//
// specRollup and checkGraph are written from that contract, not from
// rollupChildren, so they can disagree with the implementation.

// specRoll is a container's expected derived state.
type specRoll struct {
	status                         string
	total, passed, failed, skipped int
}

// specRollup derives a container's state from its children.
func specRollup(kids []*Task) specRoll {
	out := specRoll{status: StatusPending}
	if len(kids) == 0 {
		return out
	}
	var anyFailed, anyRunning, anyPending, anySkipped bool
	for _, k := range kids {
		if k.Virtual {
			// A container counts the leaves under it, never itself.
			out.total += k.Total
			out.passed += k.Passed
			out.failed += k.Failed
			out.skipped += k.Skipped
		} else {
			out.total++
			switch k.Status {
			case StatusPassed:
				out.passed++
			case StatusFailed:
				out.failed++
			case StatusSkipped:
				out.skipped++
			}
		}
		switch k.Status {
		case StatusFailed:
			anyFailed = true
		case StatusRunning:
			anyRunning = true
		case StatusPending:
			anyPending = true
		case StatusSkipped:
			anySkipped = true
		}
	}
	switch {
	case anyFailed:
		out.status = StatusFailed
	case anyRunning:
		out.status = StatusRunning
	case anyPending:
		out.status = StatusPending
	case anySkipped:
		out.status = StatusSkipped
	default:
		out.status = StatusPassed
	}
	return out
}

// checkGraph verifies the contract for one whole graph. ctx names the state
// the graph is in, so a failure says which step broke it.
func checkGraph(t *testing.T, s *Store, rootID int64, ctx string) {
	t.Helper()
	root, err := s.GetTask(rootID)
	if err != nil {
		t.Fatalf("%s: get root: %v", ctx, err)
	}
	nodes, err := listNodesTx(s.DB, rootID)
	if err != nil {
		t.Fatalf("%s: list nodes: %v", ctx, err)
	}
	byParent := map[int64][]*Task{}
	leaves := 0
	for i := range nodes {
		n := &nodes[i]
		if n.Retired {
			continue // history: no part of the current graph's state
		}
		byParent[n.ParentID] = append(byParent[n.ParentID], n)
		if !n.Virtual {
			leaves++
		}
	}
	checkRoll := func(n *Task, kids []*Task) {
		want := specRollup(kids)
		if n.Status != want.status {
			t.Errorf("%s: node %q (#%d) status=%s want %s (kids %s)", ctx, n.NodeKey, n.ID,
				n.Status, want.status, kidStatus(kids))
		}
		if n.Total != want.total || n.Passed != want.passed ||
			n.Failed != want.failed || n.Skipped != want.skipped {
			t.Errorf("%s: node %q counts T/P/F/S=%d/%d/%d/%d want %d/%d/%d/%d (kids %s)",
				ctx, n.NodeKey, n.Total, n.Passed, n.Failed, n.Skipped,
				want.total, want.passed, want.failed, want.skipped, kidStatus(kids))
		}
	}
	checkRoll(root, byParent[root.ID])
	if root.Total != leaves {
		t.Errorf("%s: root total=%d but the graph has %d active leaves", ctx, root.Total, leaves)
	}
	for i := range nodes {
		n := &nodes[i]
		if n.Retired {
			continue
		}
		if n.Virtual {
			checkRoll(n, byParent[n.ID])
			continue
		}
		if n.Attempts < 1 {
			t.Errorf("%s: real node %q has attempts=%d", ctx, n.NodeKey, n.Attempts)
			continue
		}
		runs, err := s.ListTaskRuns(n.ID)
		if err != nil {
			t.Fatalf("%s: list runs: %v", ctx, err)
		}
		var cur *TestRun
		for j := range runs {
			r := &runs[j]
			if r.Attempt > n.Attempts {
				t.Errorf("%s: node %q has run attempt %d beyond task attempts %d",
					ctx, n.NodeKey, r.Attempt, n.Attempts)
			}
			if r.Attempt == n.Attempts {
				cur = r
			} else if !TaskStatusTerminal(r.Status) {
				// A superseded attempt nobody will ever close.
				t.Errorf("%s: node %q left attempt %d in %s", ctx, n.NodeKey, r.Attempt, r.Status)
			}
		}
		if cur == nil {
			t.Errorf("%s: node %q (#%d, status %s) has no run for attempt %d",
				ctx, n.NodeKey, n.ID, n.Status, n.Attempts)
			continue
		}
		if cur.Status != n.Status || cur.Total != n.Total || cur.Passed != n.Passed ||
			cur.Failed != n.Failed || cur.Skipped != n.Skipped {
			t.Errorf("%s: node %q task=%s %d/%d/%d/%d run=%s %d/%d/%d/%d",
				ctx, n.NodeKey, n.Status, n.Total, n.Passed, n.Failed, n.Skipped,
				cur.Status, cur.Total, cur.Passed, cur.Failed, cur.Skipped)
		}
	}
}

// kidStatus renders the children's statuses for a failure message.
func kidStatus(kids []*Task) string {
	out := ""
	for _, k := range kids {
		out += fmt.Sprintf("%s=%s ", k.NodeKey, k.Status)
	}
	return out
}

// TestTaskGraphInvariantsRandomWalk drives a graph through random sequences of
// every operation the server performs on it — claims and reports, retries,
// cascades, explicit skips, rollups, re-dispatches with and without a stage —
// and checks the contract after every step. The state machine has too many
// interleavings to enumerate; the walk is the net that catches the ones no
// hand-written case thinks of.
func TestTaskGraphInvariantsRandomWalk(t *testing.T) {
	for iter := 0; iter < 60; iter++ {
		rnd := rand.New(rand.NewSource(int64(iter)))
		s := newTestTaskStore(t)
		env, commit := newGraphFixture(t, s, fmt.Sprintf("walk%d", iter))
		withUnit := rnd.Intn(2) == 0
		nodes := graphNodes(commit.ID, env.ID)
		if !withUnit {
			nodes = withoutUnit(nodes)
		}
		stored, err := s.UpsertTaskGraph(graphRoot(commit.ID, env.ID), nodes)
		if err != nil {
			t.Fatalf("iter %d: dispatch: %v", iter, err)
		}
		root := stored[0]
		var reals []*Task
		for _, n := range stored {
			if !n.Virtual {
				reals = append(reals, n)
			}
		}
		checkGraph(t, s, root.ID, fmt.Sprintf("iter %d initial", iter))

		var ops []string
		for step := 0; step < 20; step++ {
			switch rnd.Intn(10) {
			case 0, 1, 2, 3: // the scheduler: claim a ready node, report it
				task, err := s.ClaimReadyTask()
				if err != nil {
					t.Fatalf("iter %d: claim: %v", iter, err)
				}
				if task == nil {
					ops = append(ops, "claim:none")
					continue
				}
				status := []string{StatusPassed, StatusFailed, StatusSkipped}[rnd.Intn(3)]
				res := AttemptResult{Status: status, Attempt: task.Attempts, Total: 2}
				switch status {
				case StatusPassed:
					res.Passed = 2
				case StatusFailed:
					res.Passed, res.Failed = 1, 1
				case StatusSkipped:
					res.Skipped = 2
				}
				ops = append(ops, "claim+report "+task.NodeKey+" "+status)
				if _, err := s.FinishAttempt(task.ID, res); err != nil {
					t.Fatalf("iter %d step %d (%v): report: %v", iter, step, ops, err)
				}
			case 4: // a cascade from a random node
				id := reals[rnd.Intn(len(reals))].ID
				ops = append(ops, fmt.Sprintf("skipdependents %d", id))
				if err := s.SkipDependents(root.ID, id, "upstream failed"); err != nil {
					t.Fatalf("iter %d step %d (%v): skipdependents: %v", iter, step, ops, err)
				}
			case 5: // a node skipped for a reason of its own
				id := reals[rnd.Intn(len(reals))].ID
				ops = append(ops, fmt.Sprintf("skiptask %d", id))
				if err := s.SkipTask(id, "no command"); err != nil {
					t.Fatalf("iter %d step %d (%v): skiptask: %v", iter, step, ops, err)
				}
			case 6: // an explicit rollup
				ops = append(ops, "rollup")
				if err := s.RollupTaskTree(root.ID); err != nil {
					t.Fatalf("iter %d: rollup: %v", iter, err)
				}
			case 7: // a report from outside, against a random node
				id := reals[rnd.Intn(len(reals))].ID
				ops = append(ops, fmt.Sprintf("report %d", id))
				if _, err := s.FinishAttempt(id, AttemptResult{Status: StatusPassed, Passed: 1, Total: 1}); err != nil {
					t.Fatalf("iter %d step %d (%v): report: %v", iter, step, ops, err)
				}
			case 8: // a re-dispatch of the same yaml
				ops = append(ops, "redispatch")
				if _, err := s.UpsertTaskGraph(graphRoot(commit.ID, env.ID), nodes); err != nil {
					t.Fatalf("iter %d: redispatch: %v", iter, err)
				}
			case 9: // a re-dispatch that drops the unit stage (it is retired)
				ops = append(ops, "redispatch-no-unit")
				if _, err := s.UpsertTaskGraph(graphRoot(commit.ID, env.ID),
					withoutUnit(graphNodes(commit.ID, env.ID))); err != nil {
					t.Fatalf("iter %d: redispatch reduced: %v", iter, err)
				}
			}
			checkGraph(t, s, root.ID, fmt.Sprintf("iter %d step %d ops=%v", iter, step, ops))
		}
	}
}

// withoutUnit drops the unit stage from a node list, the way a yaml that no
// longer defines it does. Only the unit node is removed: nothing depends on
// it, so the remaining dependency placeholders still resolve.
func withoutUnit(nodes []TaskNode) []TaskNode {
	out := make([]TaskNode, 0, len(nodes))
	for _, n := range nodes {
		if n.Task.Kind == TaskKindUnit {
			continue
		}
		out = append(out, n)
	}
	return out
}
