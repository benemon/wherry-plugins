package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/benemon/wherry/pluginsdk"
	"github.com/hashicorp/nomad/api"
)

// shortLookupWait shortens the delay between a waiting Lookup's observations for one test.
func shortLookupWait(t *testing.T) {
	previous := lookupWait
	lookupWait = 20 * time.Millisecond
	t.Cleanup(func() { lookupWait = previous })
}

// pendingRunID is the run whose job the placement fixtures serve, as poolPrefix names it.
const pendingRunID = "run-pending"

const pendingJobID = poolPrefix + pendingRunID

// pendingJob is an agent job at modify index 20, as Info returns it, with status and stop.
func pendingJob(t *testing.T, status string, stop bool) []byte {
	t.Helper()
	data, err := json.Marshal(&api.Job{
		ID: new(pendingJobID), Status: new(status), Stop: new(stop), JobModifyIndex: new(uint64(20)),
		Meta:       map[string]string{"run_id": pendingRunID, "attempt": "2", "profile": "container", "agent": "coder"},
		TaskGroups: []*api.TaskGroup{{Name: new(taskName), Tasks: []*api.Task{{Name: taskName, Driver: "raw_exec"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func encoded(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// metricA and metricB are distinct failed-placement metrics, so a reason shows which evaluation it came from.
var (
	metricA = &api.AllocationMetric{NodesEvaluated: 3, NodesExhausted: 3, NodePool: "default", NodesInPool: 3, DimensionExhausted: map[string]int{"cpu": 3}}
	metricB = &api.AllocationMetric{NodesEvaluated: 2, NodesExhausted: 2, NodePool: "default", NodesInPool: 2, DimensionExhausted: map[string]int{"memory": 2}}
)

const (
	reasonA = "3 nodes evaluated, 0 filtered, 3 exhausted; cpu exhausted on 3 nodes"
	reasonB = "2 nodes evaluated, 0 filtered, 2 exhausted; memory exhausted on 2 nodes"
)

// eval is an evaluation of the pending job at modify index, created at create, with status, queued agent allocations
// and the agent group's metric.
func eval(id string, index, create uint64, status string, queued int, metric *api.AllocationMetric) *api.Evaluation {
	evaluation := &api.Evaluation{ID: id, JobID: pendingJobID, JobModifyIndex: index, CreateIndex: create, Status: status, QueuedAllocations: map[string]int{taskName: queued}}
	if metric != nil {
		evaluation.FailedTGAllocs = map[string]*api.AllocationMetric{taskName: metric}
	}
	return evaluation
}

func failWith(code int) http.HandlerFunc {
	return func(response http.ResponseWriter, _ *http.Request) { http.Error(response, http.StatusText(code), code) }
}

// lookupOutcome is what one Lookup of the pending job decided: a state with its class and reason, a wait (the
// context's deadline), or an observation error with its class.
type lookupOutcome struct {
	state         pluginsdk.JobState
	class, reason string
	wait          bool
	failed        string
}

func lookupOnce(t *testing.T, profile *Nomad) (pluginsdk.Job, lookupOutcome) {
	t.Helper()
	shortLookupWait(t)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	job, err := profile.Lookup(ctx, pendingRunID)
	var failure *pluginsdk.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return job, lookupOutcome{wait: true}
	case errors.As(err, &failure):
		return job, lookupOutcome{failed: failure.Op + "/" + failure.Class}
	case err != nil:
		t.Fatalf("lookup: %v", err)
	}
	return job, lookupOutcome{state: job.State, class: job.Class, reason: job.Reason}
}

// TestLookupDecisionOrder walks mch5's Nomad rows: allocations decide first, then a stopped job, then the current
// evaluation's queue or blocked status, then the blocked evaluation it names; anything else waits, and an essential read
// that fails is an observation error.
func TestLookupDecisionOrder(t *testing.T) {
	none := serve([]byte(`[]`))
	pending := func(reason string) lookupOutcome { return lookupOutcome{state: pluginsdk.JobPending, reason: reason} }
	wait := lookupOutcome{wait: true}
	running := serve([]byte(`[{"ID":"alloc-1","ClientStatus":"running","TaskStates":{"agent":{"State":"running","StartedAt":"2026-11-01T00:00:00Z"}}}]`))
	type test struct {
		name   string
		job    []byte
		allocs http.HandlerFunc
		evals  http.HandlerFunc
		// info answers GET /v1/evaluation/blocked-1; nil fails the test if it is read.
		info http.HandlerFunc
		want lookupOutcome
	}
	evals := func(list ...*api.Evaluation) http.HandlerFunc { return serve(encoded(t, list)) }
	tests := []test{
		{"no allocation, queued, complete", nil, none, evals(eval("e1", 20, 21, api.EvalStatusComplete, 1, metricA)), nil, pending(reasonA)},
		{"no allocation, queued, failed", nil, none, evals(eval("e1", 20, 21, api.EvalStatusFailed, 1, metricA)), nil, pending(reasonA)},
		{"no allocation, queued, canceled", nil, none, evals(eval("e1", 20, 21, api.EvalStatusCancelled, 1, metricA)), nil, pending(reasonA)},
		{"no allocation, queued and blocked", nil, none, evals(eval("e1", 20, 21, api.EvalStatusBlocked, 1, metricA)), nil, pending(reasonA)},
		{"no allocation, blocked, none queued", nil, none, evals(eval("e1", 20, 21, api.EvalStatusBlocked, 0, metricA)), nil, pending(reasonA)},
		{"no evaluation", nil, none, evals(), nil, wait},
		{"stale evaluation only", nil, none, evals(eval("e0", 19, 19, api.EvalStatusBlocked, 1, metricA)), nil, wait},
		{"current pending evaluation", nil, none, evals(eval("e1", 20, 21, api.EvalStatusPending, 0, nil)), nil, wait},
		{"stopped before placement", pendingJob(t, "pending", true), none, nil, nil, lookupOutcome{state: pluginsdk.JobNeverStarted, class: classStopped}},
		{"dead before placement", pendingJob(t, "dead", false), none, nil, nil, lookupOutcome{state: pluginsdk.JobNeverStarted, class: classStopped}},
		{"stopped with a running allocation", pendingJob(t, "running", true), running, nil, nil, lookupOutcome{state: pluginsdk.JobRunning}},
		{"allocation with a stale blocked evaluation", nil, running, nil, nil, lookupOutcome{state: pluginsdk.JobRunning}},
		{"allocations fail", nil, failWith(http.StatusInternalServerError), nil, nil, lookupOutcome{failed: "list allocations/unavailable"}},
		{"evaluations fail", nil, none, failWith(http.StatusForbidden), nil, lookupOutcome{failed: "list evaluations/denied"}},
		{"blocked evaluation named, blocked", nil, none, evals(naming(eval("e1", 20, 21, api.EvalStatusComplete, 0, metricA))),
			serve(encoded(t, eval("blocked-1", 20, 22, api.EvalStatusBlocked, 0, metricB))), pending(reasonB)},
		{"blocked evaluation named, missing", nil, none, evals(naming(eval("e1", 20, 21, api.EvalStatusComplete, 0, metricA))), failWith(http.StatusNotFound), wait},
		{"blocked evaluation named, complete", nil, none, evals(naming(eval("e1", 20, 21, api.EvalStatusComplete, 0, metricA))),
			serve(encoded(t, eval("blocked-1", 20, 22, api.EvalStatusComplete, 0, metricB))), wait},
		{"blocked evaluation named, timeout", nil, none, evals(naming(eval("e1", 20, 21, api.EvalStatusComplete, 0, metricA))), failWith(http.StatusGatewayTimeout), lookupOutcome{failed: "read evaluation/timeout"}},
		{"blocked evaluation named, denied", nil, none, evals(naming(eval("e1", 20, 21, api.EvalStatusComplete, 0, metricA))), failWith(http.StatusForbidden), lookupOutcome{failed: "read evaluation/denied"}},
	}
	// None queued and not blocked waits whatever the current evaluation's status and metric.
	for _, status := range []string{api.EvalStatusComplete, api.EvalStatusFailed, api.EvalStatusCancelled, "unknown-status", ""} {
		for name, metric := range map[string]*api.AllocationMetric{"populated": metricA, "all-zero": {}} {
			tests = append(tests, test{"none queued, " + name + " metric, status " + cmpOr(status, "none"), nil, none, evals(eval("e1", 20, 21, status, 0, metric)), nil, wait})
		}
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			job := test.job
			if job == nil {
				job = pendingJob(t, "pending", false)
			}
			routes := map[string]http.HandlerFunc{
				"GET /v1/job/" + pendingJobID:                  serve(job),
				"GET /v1/job/" + pendingJobID + "/allocations": test.allocs,
			}
			if test.evals != nil {
				routes["GET /v1/job/"+pendingJobID+"/evaluations"] = test.evals
			}
			if test.info != nil {
				routes["GET /v1/evaluation/blocked-1"] = test.info
			}
			profile, requests := nomadServer(t, routes)
			found, got := lookupOnce(t, profile)
			if got != test.want {
				t.Fatalf("lookup = %+v; want %+v (requests %v)", got, test.want, requests())
			}
			if got.state == pluginsdk.JobPending || got.state == pluginsdk.JobNeverStarted {
				if h := decoded(t, found.Handle); h.Job != pendingJobID || h.Alloc != "" || found.Attempt != 2 || found.Profile != "container" || found.Agent != "coder" {
					t.Fatalf("handle %#v, job %+v; want the job's handle with no allocation and its metadata", h, found)
				}
			}
			for _, request := range requests() {
				if test.allocs != nil && test.evals == nil && strings.Contains(request, "evaluation") {
					t.Fatalf("requests %v; want no evaluation read once allocations or the job decide", requests())
				}
				if test.info == nil && strings.HasPrefix(request, "GET /v1/evaluation/") {
					t.Fatalf("requests %v; want no evaluation read by ID", requests())
				}
			}
		})
	}
}

func cmpOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// naming makes e name the blocked evaluation blocked-1.
func naming(e *api.Evaluation) *api.Evaluation {
	e.BlockedEval = "blocked-1"
	return e
}

// TestLookupReasonComesFromTheCurrentEvaluation: of a stale evaluation with metric A and the current one blocked with
// metric B, the reason is B's; a referenced blocked evaluation's metric, and its StatusDescription, beat the current
// one's.
func TestLookupReasonComesFromTheCurrentEvaluation(t *testing.T) {
	stale := eval("e0", 19, 30, api.EvalStatusBlocked, 1, metricA)
	current := eval("e1", 20, 21, api.EvalStatusBlocked, 0, metricB)
	profile, _ := nomadServer(t, map[string]http.HandlerFunc{
		"GET /v1/job/" + pendingJobID:                  serve(pendingJob(t, "pending", false)),
		"GET /v1/job/" + pendingJobID + "/allocations": serve([]byte(`[]`)),
		"GET /v1/job/" + pendingJobID + "/evaluations": serve(encoded(t, []*api.Evaluation{stale, current})),
	})
	if _, got := lookupOnce(t, profile); got != (lookupOutcome{state: pluginsdk.JobPending, reason: reasonB}) {
		t.Fatalf("lookup = %+v; want pending with B's reason %q", got, reasonB)
	}
	referring := eval("e1", 20, 21, api.EvalStatusComplete, 0, metricA)
	referring.BlockedEval, referring.StatusDescription = "blocked-1", "the current evaluation's description"
	blocked := eval("blocked-1", 20, 22, api.EvalStatusBlocked, 0, nil)
	blocked.StatusDescription = "created to place remaining allocations"
	profile, _ = nomadServer(t, map[string]http.HandlerFunc{
		"GET /v1/job/" + pendingJobID:                  serve(pendingJob(t, "pending", false)),
		"GET /v1/job/" + pendingJobID + "/allocations": serve([]byte(`[]`)),
		"GET /v1/job/" + pendingJobID + "/evaluations": serve(encoded(t, []*api.Evaluation{referring})),
		"GET /v1/evaluation/blocked-1":                 serve(encoded(t, blocked)),
	})
	if _, got := lookupOnce(t, profile); got != (lookupOutcome{state: pluginsdk.JobPending, reason: blocked.StatusDescription}) {
		t.Fatalf("lookup = %+v; want pending with the blocked evaluation's description", got)
	}
}

// TestLookupReportsCapturedUnplacedJobsPending replays the two captures from a Nomad 2.0.7 dev agent: a job for a
// datacenter no node is in, and one asking for more memory than any node has. Each has no allocation and a current
// evaluation that is blocked, so Lookup reports pending at once with the reason formatted from its metric.
func TestLookupReportsCapturedUnplacedJobsPending(t *testing.T) {
	for capture, want := range map[string]string{
		"unplaced-datacenter": "0 nodes evaluated, 0 filtered, 0 exhausted; node pool default has no nodes",
		"unplaced-memory":     "1 nodes evaluated, 0 filtered, 1 exhausted; memory exhausted on 1 nodes",
	} {
		t.Run(capture, func(t *testing.T) {
			id := poolPrefix + "capture-" + capture
			profile, requests := nomadServer(t, map[string]http.HandlerFunc{
				"GET /v1/job/" + id:                  serve(captured(t, capture+".job.json")),
				"GET /v1/job/" + id + "/allocations": serve([]byte(`[]`)),
				"GET /v1/job/" + id + "/evaluations": serve(captured(t, capture+".evaluations.json")),
			})
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			job, err := profile.Lookup(ctx, "capture-"+capture)
			if err != nil || job.State != pluginsdk.JobPending || job.Reason != want || job.Profile != "container" || job.Attempt != 1 {
				t.Fatalf("lookup = %#v, %v; want pending with reason %q", job, err, want)
			}
			if h := decoded(t, job.Handle); h.Job != id || h.Alloc != "" {
				t.Fatalf("handle %#v; want the job with no allocation", h)
			}
			want := []string{"GET /v1/job/" + id, "GET /v1/job/" + id + "/allocations", "GET /v1/job/" + id + "/evaluations"}
			if got := requests(); !slices.Equal(got, want) {
				t.Fatalf("requests %v; want %v, with no read of the blocked evaluation by ID", got, want)
			}
		})
	}
}

// TestLookupWaitsForAPlacedTaskToStart: an allocation Nomad has placed whose task has not started is not pending;
// Lookup waits and reports running once the task starts, with the allocation in its handle.
func TestLookupWaitsForAPlacedTaskToStart(t *testing.T) {
	var mu sync.Mutex
	reads := 0
	profile, _ := nomadServer(t, map[string]http.HandlerFunc{
		"GET /v1/job/" + pendingJobID: serve(pendingJob(t, "running", false)),
		"GET /v1/job/" + pendingJobID + "/allocations": func(response http.ResponseWriter, _ *http.Request) {
			mu.Lock()
			reads++
			read := reads
			mu.Unlock()
			if read < 3 {
				_, _ = response.Write([]byte(`[{"ID":"alloc-1","ClientStatus":"pending","TaskStates":{"agent":{"State":"pending"}}}]`))
				return
			}
			_, _ = response.Write([]byte(`[{"ID":"alloc-1","ClientStatus":"running","TaskStates":{"agent":{"State":"running","StartedAt":"2026-11-01T00:00:00Z"}}}]`))
		},
	})
	shortLookupWait(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	job, err := profile.Lookup(ctx, pendingRunID)
	if err != nil || job.State != pluginsdk.JobRunning || decoded(t, job.Handle).Alloc != "alloc-1" {
		t.Fatalf("lookup = %#v, %v; want running with alloc-1 after waiting", job, err)
	}
	if reads != 3 {
		t.Fatalf("allocations read %d times; want 3", reads)
	}
}

// TestLookupNeverStartedCarriesTheTerminalsClassAndReason: a terminal allocation whose task never started is never
// started with the class and reason terminalOf gives Follow.
func TestLookupNeverStartedCarriesTheTerminalsClassAndReason(t *testing.T) {
	const id = poolPrefix + "capture-pull"
	profile, _ := nomadServer(t, map[string]http.HandlerFunc{
		"GET /v1/job/" + id:                  serve(captured(t, "pull-failure.job.json")),
		"GET /v1/job/" + id + "/allocations": serve(captured(t, "pull-failure.allocations.json")),
	})
	job, err := profile.Lookup(context.Background(), "capture-pull")
	if err != nil {
		t.Fatal(err)
	}
	var alloc api.Allocation
	if err := json.Unmarshal(captured(t, "pull-failure.allocation.json"), &alloc); err != nil {
		t.Fatal(err)
	}
	want := terminalOf(&alloc)
	if job.State != pluginsdk.JobNeverStarted || want.Class == "" || want.Reason == "" || job.Class != want.Class || job.Reason != want.Reason {
		t.Fatalf("lookup = %#v; want never started with class %q and reason %q", job, want.Class, want.Reason)
	}
}

// TestPendingReason checks the formatter's exact output: every counter named, fragments in a fixed order, zero counts
// skipped, the fallback only when nothing would be named.
func TestPendingReason(t *testing.T) {
	const description = "created to place remaining allocations"
	for _, test := range []struct {
		name        string
		metric      *api.AllocationMetric
		description string
		want        string
	}{
		{"nil metric", nil, description, description},
		{"nil metric, no description", nil, "", ""},
		{"empty metric", &api.AllocationMetric{}, description, description},
		{"empty metric, no description", &api.AllocationMetric{}, "", ""},
		{"counters zero, pool has nodes", &api.AllocationMetric{NodePool: "default", NodesInPool: 2}, description, description},
		{"quota only", &api.AllocationMetric{QuotaExhausted: []string{"team-b", "team-a"}}, description,
			"0 nodes evaluated, 0 filtered, 0 exhausted; quota team-a exhausted; quota team-b exhausted"},
		{"one zero datacenter", &api.AllocationMetric{NodesAvailable: map[string]int{"dc1": 1, "dc2": 0}}, description,
			"0 nodes evaluated, 0 filtered, 0 exhausted; no nodes available in datacenter dc2"},
		{"empty pool", &api.AllocationMetric{NodePool: "gpu"}, description, "0 nodes evaluated, 0 filtered, 0 exhausted; node pool gpu has no nodes"},
		{"zero-count entries skipped", &api.AllocationMetric{NodesEvaluated: 4, NodesFiltered: 1, NodesExhausted: 3, NodesInPool: 4, NodePool: "default",
			NodesAvailable: map[string]int{"dc1": 4}, DimensionExhausted: map[string]int{"cpu": 0, "memory": 3}, ConstraintFiltered: map[string]int{"${attr.kernel.name} = windows": 0},
			ClassFiltered: map[string]int{"gpu": 0}, ClassExhausted: map[string]int{"large": 0}}, description,
			"4 nodes evaluated, 1 filtered, 3 exhausted; memory exhausted on 3 nodes"},
		{"every fragment", &api.AllocationMetric{NodesEvaluated: 5, NodesFiltered: 2, NodesExhausted: 3, NodesInPool: 0, NodePool: "edge",
			NodesAvailable: map[string]int{"dc2": 0, "dc1": 0}, DimensionExhausted: map[string]int{"memory": 2, "cpu": 1},
			ConstraintFiltered: map[string]int{"${node.class} = x": 2}, ClassFiltered: map[string]int{"small": 1}, ClassExhausted: map[string]int{"large": 3},
			QuotaExhausted: []string{"q1"}, ResourcesExhausted: map[string]*api.Resources{"agent": {MemoryMB: new(4096)}},
			Scores: map[string]float64{"node": 0.5}, CoalescedFailures: 7}, description,
			"5 nodes evaluated, 2 filtered, 3 exhausted; node pool edge has no nodes; no nodes available in datacenter dc1; no nodes available in datacenter dc2; " +
				"cpu exhausted on 1 nodes; memory exhausted on 2 nodes; constraint ${node.class} = x filtered 2; class small filtered 1; class large exhausted 3; quota q1 exhausted"},
	} {
		t.Run(test.name, func(t *testing.T) {
			evaluation := &api.Evaluation{StatusDescription: test.description}
			if test.metric != nil {
				evaluation.FailedTGAllocs = map[string]*api.AllocationMetric{taskName: test.metric}
			}
			if got := pendingReason(evaluation); got != test.want {
				t.Fatalf("reason %q; want %q", got, test.want)
			}
		})
	}
}

// TestPendingReasonSortsWhateverTheEnumerationOrder sets enumerate to list every map's keys and the quotas in reverse
// order: Go's map order is unspecified, so the exact-output cases alone catch an unsorted formatter only by chance.
func TestPendingReasonSortsWhateverTheEnumerationOrder(t *testing.T) {
	previous := enumerate
	enumerate = func(keys []string) []string {
		slices.Sort(keys)
		slices.Reverse(keys)
		return keys
	}
	t.Cleanup(func() { enumerate = previous })
	m := &api.AllocationMetric{NodesEvaluated: 3, NodePool: "default", NodesInPool: 3,
		NodesAvailable: map[string]int{"a": 0, "b": 0, "c": 0}, DimensionExhausted: map[string]int{"cpu": 1, "disk": 1, "memory": 1},
		ConstraintFiltered: map[string]int{"c1": 1, "c2": 1}, ClassFiltered: map[string]int{"f1": 1, "f2": 1}, ClassExhausted: map[string]int{"x1": 1, "x2": 1},
		QuotaExhausted: []string{"q1", "q2", "q3"}}
	want := "3 nodes evaluated, 0 filtered, 0 exhausted; no nodes available in datacenter a; no nodes available in datacenter b; no nodes available in datacenter c; " +
		"cpu exhausted on 1 nodes; disk exhausted on 1 nodes; memory exhausted on 1 nodes; constraint c1 filtered 1; constraint c2 filtered 1; " +
		"class f1 filtered 1; class f2 filtered 1; class x1 exhausted 1; class x2 exhausted 1; quota q1 exhausted; quota q2 exhausted; quota q3 exhausted"
	if got := pendingReason(&api.Evaluation{FailedTGAllocs: map[string]*api.AllocationMetric{taskName: m}}); got != want {
		t.Fatalf("reason %q; want %q", got, want)
	}
}

// TestDispatchReturnsAtRegistration: Dispatch registers the job and returns created with a handle naming no
// allocation, reading neither allocations nor evaluations, and reports no placement sample; a job already registered
// returns its handle at once.
func TestDispatchReturnsAtRegistration(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "new", true: "existing"}[existing], func(t *testing.T) {
			routes := map[string]http.HandlerFunc{
				"PUT /v1/var/nomad/jobs/" + pendingJobID: serve([]byte(`{}`)),
				"GET /v1/job/" + pendingJobID:            jobNotFound,
				"PUT /v1/jobs":                           serve([]byte(`{"EvalID":"eval-1","EvalCreateIndex":10,"JobModifyIndex":10}`)),
			}
			if existing {
				routes["GET /v1/job/"+pendingJobID] = serve(pendingJob(t, "pending", false))
			}
			profile, requests := nomadServer(t, routes)
			run := testRun("registered")
			run.ID = pendingRunID
			outcome, dispatched, err := profile.Dispatch(context.Background(), run)
			if err != nil || outcome != pluginsdk.OutcomeCreated || dispatched.Name != pendingJobID {
				t.Fatalf("dispatch = %v, %q, %v; want created", outcome, dispatched.Name, err)
			}
			if h := decoded(t, dispatched); h != (handle{V: 1, Job: pendingJobID}) {
				t.Fatalf("handle %#v; want version 1 for the job with no allocation", h)
			}
			for _, request := range requests() {
				if strings.Contains(request, "/allocation") || strings.Contains(request, "/evaluation") {
					t.Fatalf("requests %v; want none of allocations or evaluations", requests())
				}
			}
			if registered := slices.Contains(requests(), "PUT /v1/jobs"); registered == existing {
				t.Fatalf("requests %v; want a registration only for a new job", requests())
			}
			for family := range samples(t, profile) {
				if strings.HasPrefix(family, "wherry_plugin_placement_seconds") {
					t.Fatalf("sample %s; want no placement family", family)
				}
			}
		})
	}
}

// TestFollowAndCollectRefuseAHandleWithoutAnAllocation: Dispatch's handle names no allocation, and Follow and Collect
// fail on it with op read allocation before any request.
func TestFollowAndCollectRefuseAHandleWithoutAnAllocation(t *testing.T) {
	profile, requests := nomadServer(t, nil)
	unplaced := encodeHandle(pendingJobID, "", "raw_exec")
	_, followErr := profile.Follow(context.Background(), unplaced, 0, make(chan pluginsdk.Line, 1))
	collectErr := profile.Collect(context.Background(), unplaced, wide, make(chan pluginsdk.Output, 1))
	for name, err := range map[string]error{"follow": followErr, "collect": collectErr} {
		var failure *pluginsdk.Error
		if !errors.As(err, &failure) || failure.Op != "read allocation" {
			t.Fatalf("%s = %v; want a pluginsdk.Error with op read allocation", name, err)
		}
	}
	if got := requests(); len(got) != 0 {
		t.Fatalf("requests %v; want none", got)
	}
}

// TestStopWithoutAnAllocation: Stop with Dispatch's handle deregisters the job by its ID, without a purge, a job
// already gone counting as stopped, and returns once no allocation is non-terminal, including one Nomad placed between
// Dispatch and Stop.
func TestStopWithoutAnAllocation(t *testing.T) {
	placed := `[{"ID":"alloc-1","ClientStatus":"running","TaskStates":{"agent":{"State":"running"}}}]`
	ended := `[{"ID":"alloc-1","ClientStatus":"complete","TaskStates":{"agent":{"State":"dead"}}}]`
	for _, test := range []struct {
		name       string
		deregister http.HandlerFunc
		listings   []string
	}{
		{"queued", serve([]byte(`{"EvalID":"eval-2"}`)), []string{`[]`}},
		{"already gone", jobNotFound, []string{`[]`}},
		{"placed between dispatch and stop", serve([]byte(`{"EvalID":"eval-2"}`)), []string{placed, placed, ended}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var mu sync.Mutex
			var purge []string
			listed := 0
			profile, requests := nomadServer(t, map[string]http.HandlerFunc{
				"DELETE /v1/job/" + pendingJobID: func(response http.ResponseWriter, request *http.Request) {
					mu.Lock()
					purge = append(purge, request.URL.Query().Get("purge"))
					mu.Unlock()
					test.deregister(response, request)
				},
				"GET /v1/job/" + pendingJobID + "/allocations": func(response http.ResponseWriter, _ *http.Request) {
					mu.Lock()
					listing := test.listings[min(listed, len(test.listings)-1)]
					listed++
					mu.Unlock()
					_, _ = response.Write([]byte(listing))
				},
			})
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := profile.Stop(ctx, encodeHandle(pendingJobID, "", "raw_exec")); err != nil {
				t.Fatalf("stop: %v", err)
			}
			want := []string{"DELETE /v1/job/" + pendingJobID}
			for range test.listings {
				want = append(want, "GET /v1/job/"+pendingJobID+"/allocations")
			}
			if got := requests(); !slices.Equal(got, want) || slices.Contains(purge, "true") {
				t.Fatalf("requests %v, purge %v; want %v without a purge", got, purge, want)
			}
		})
	}
}
