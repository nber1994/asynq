// Copyright 2020 Kentaro Hibino. All rights reserved.
// Use of this source code is governed by a MIT license
// that can be found in the LICENSE file.

package rdb

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/go-redis/redis/v7"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	h "github.com/hibiken/asynq/internal/asynqtest"
	"github.com/hibiken/asynq/internal/base"
	"github.com/rs/xid"
)

// TODO(hibiken): Get Redis address and db number from ENV variables.
func setup(t *testing.T) *RDB {
	t.Helper()
	r := NewRDB(redis.NewClient(&redis.Options{
		Addr: "localhost:6379",
		DB:   13,
	}))
	// Start each test with a clean slate.
	h.FlushDB(t, r.client)
	return r
}

func TestEnqueue(t *testing.T) {
	r := setup(t)
	t1 := h.NewTaskMessage("send_email", map[string]interface{}{"to": "exampleuser@gmail.com", "from": "noreply@example.com"})
	t2 := h.NewTaskMessage("generate_csv", map[string]interface{}{})
	t2.Queue = "csv"
	t3 := h.NewTaskMessage("sync", nil)
	t3.Queue = "low"

	tests := []struct {
		msg *base.TaskMessage
	}{
		{t1},
		{t2},
		{t3},
	}

	for _, tc := range tests {
		h.FlushDB(t, r.client) // clean up db before each test case.

		err := r.Enqueue(tc.msg)
		if err != nil {
			t.Errorf("(*RDB).Enqueue(msg) = %v, want nil", err)
		}

		qkey := base.QueueKey(tc.msg.Queue)
		gotEnqueued := h.GetEnqueuedMessages(t, r.client, tc.msg.Queue)
		if len(gotEnqueued) != 1 {
			t.Errorf("%q has length %d, want 1", qkey, len(gotEnqueued))
			continue
		}
		if diff := cmp.Diff(tc.msg, gotEnqueued[0]); diff != "" {
			t.Errorf("persisted data differed from the original input (-want, +got)\n%s", diff)
		}
		if !r.client.SIsMember(base.AllQueues, qkey).Val() {
			t.Errorf("%q is not a member of SET %q", qkey, base.AllQueues)
		}
	}
}

func TestEnqueueUnique(t *testing.T) {
	r := setup(t)
	m1 := base.TaskMessage{
		ID:        xid.New(),
		Type:      "email",
		Payload:   map[string]interface{}{"user_id": 123},
		Queue:     base.DefaultQueueName,
		UniqueKey: "email:user_id=123:default",
	}

	tests := []struct {
		msg *base.TaskMessage
		ttl time.Duration // uniqueness ttl
	}{
		{&m1, time.Minute},
	}

	for _, tc := range tests {
		h.FlushDB(t, r.client) // clean up db before each test case.

		err := r.EnqueueUnique(tc.msg, tc.ttl)
		if err != nil {
			t.Errorf("First message: (*RDB).EnqueueUnique(%v, %v) = %v, want nil",
				tc.msg, tc.ttl, err)
			continue
		}

		got := r.EnqueueUnique(tc.msg, tc.ttl)
		if got != ErrDuplicateTask {
			t.Errorf("Second message: (*RDB).EnqueueUnique(%v, %v) = %v, want %v",
				tc.msg, tc.ttl, got, ErrDuplicateTask)
			continue
		}

		gotTTL := r.client.TTL(tc.msg.UniqueKey).Val()
		if !cmp.Equal(tc.ttl.Seconds(), gotTTL.Seconds(), cmpopts.EquateApprox(0, 1)) {
			t.Errorf("TTL %q = %v, want %v", tc.msg.UniqueKey, gotTTL, tc.ttl)
			continue
		}
	}
}

func TestDequeue(t *testing.T) {
	r := setup(t)
	t1 := h.NewTaskMessage("send_email", map[string]interface{}{"subject": "hello!"})
	t2 := h.NewTaskMessage("export_csv", nil)
	t3 := h.NewTaskMessage("reindex", nil)

	tests := []struct {
		enqueued       map[string][]*base.TaskMessage
		args           []string // list of queues to query
		want           *base.TaskMessage
		err            error
		wantEnqueued   map[string][]*base.TaskMessage
		wantInProgress []*base.TaskMessage
	}{
		{
			enqueued: map[string][]*base.TaskMessage{
				"default": {t1},
			},
			args: []string{"default"},
			want: t1,
			err:  nil,
			wantEnqueued: map[string][]*base.TaskMessage{
				"default": {},
			},
			wantInProgress: []*base.TaskMessage{t1},
		},
		{
			enqueued: map[string][]*base.TaskMessage{
				"default": {},
			},
			args: []string{"default"},
			want: nil,
			err:  ErrNoProcessableTask,
			wantEnqueued: map[string][]*base.TaskMessage{
				"default": {},
			},
			wantInProgress: []*base.TaskMessage{},
		},
		{
			enqueued: map[string][]*base.TaskMessage{
				"default":  {t1},
				"critical": {t2},
				"low":      {t3},
			},
			args: []string{"critical", "default", "low"},
			want: t2,
			err:  nil,
			wantEnqueued: map[string][]*base.TaskMessage{
				"default":  {t1},
				"critical": {},
				"low":      {t3},
			},
			wantInProgress: []*base.TaskMessage{t2},
		},
		{
			enqueued: map[string][]*base.TaskMessage{
				"default":  {t1},
				"critical": {},
				"low":      {t2, t3},
			},
			args: []string{"critical", "default", "low"},
			want: t1,
			err:  nil,
			wantEnqueued: map[string][]*base.TaskMessage{
				"default":  {},
				"critical": {},
				"low":      {t2, t3},
			},
			wantInProgress: []*base.TaskMessage{t1},
		},
		{
			enqueued: map[string][]*base.TaskMessage{
				"default":  {},
				"critical": {},
				"low":      {},
			},
			args: []string{"critical", "default", "low"},
			want: nil,
			err:  ErrNoProcessableTask,
			wantEnqueued: map[string][]*base.TaskMessage{
				"default":  {},
				"critical": {},
				"low":      {},
			},
			wantInProgress: []*base.TaskMessage{},
		},
	}

	for _, tc := range tests {
		h.FlushDB(t, r.client) // clean up db before each test case
		for queue, msgs := range tc.enqueued {
			h.SeedEnqueuedQueue(t, r.client, msgs, queue)
		}

		got, err := r.Dequeue(tc.args...)
		if !cmp.Equal(got, tc.want) || err != tc.err {
			t.Errorf("(*RDB).Dequeue(%v) = %v, %v; want %v, %v",
				tc.args, got, err, tc.want, tc.err)
			continue
		}

		for queue, want := range tc.wantEnqueued {
			gotEnqueued := h.GetEnqueuedMessages(t, r.client, queue)
			if diff := cmp.Diff(want, gotEnqueued, h.SortMsgOpt); diff != "" {
				t.Errorf("mismatch found in %q: (-want,+got):\n%s", base.QueueKey(queue), diff)
			}
		}

		gotInProgress := h.GetInProgressMessages(t, r.client)
		if diff := cmp.Diff(tc.wantInProgress, gotInProgress, h.SortMsgOpt); diff != "" {
			t.Errorf("mismatch found in %q: (-want,+got):\n%s", base.InProgressQueue, diff)
		}
	}
}

func TestDone(t *testing.T) {
	r := setup(t)
	t1 := h.NewTaskMessage("send_email", nil)
	t2 := h.NewTaskMessage("export_csv", nil)
	t3 := &base.TaskMessage{
		ID:        xid.New(),
		Type:      "reindex",
		Payload:   nil,
		UniqueKey: "reindex:nil:default",
		Queue:     "default",
	}

	tests := []struct {
		inProgress     []*base.TaskMessage // initial state of the in-progress list
		target         *base.TaskMessage   // task to remove
		wantInProgress []*base.TaskMessage // final state of the in-progress list
	}{
		{
			inProgress:     []*base.TaskMessage{t1, t2},
			target:         t1,
			wantInProgress: []*base.TaskMessage{t2},
		},
		{
			inProgress:     []*base.TaskMessage{t1},
			target:         t1,
			wantInProgress: []*base.TaskMessage{},
		},
		{
			inProgress:     []*base.TaskMessage{t1, t2, t3},
			target:         t3,
			wantInProgress: []*base.TaskMessage{t1, t2},
		},
	}

	for _, tc := range tests {
		h.FlushDB(t, r.client) // clean up db before each test case
		h.SeedInProgressQueue(t, r.client, tc.inProgress)
		for _, msg := range tc.inProgress {
			// Set uniqueness lock if unique key is present.
			if len(msg.UniqueKey) > 0 {
				err := r.client.SetNX(msg.UniqueKey, msg.ID.String(), time.Minute).Err()
				if err != nil {
					t.Fatal(err)
				}
			}
		}

		err := r.Done(tc.target)
		if err != nil {
			t.Errorf("(*RDB).Done(task) = %v, want nil", err)
			continue
		}

		gotInProgress := h.GetInProgressMessages(t, r.client)
		if diff := cmp.Diff(tc.wantInProgress, gotInProgress, h.SortMsgOpt); diff != "" {
			t.Errorf("mismatch found in %q: (-want, +got):\n%s", base.InProgressQueue, diff)
			continue
		}

		processedKey := base.ProcessedKey(time.Now())
		gotProcessed := r.client.Get(processedKey).Val()
		if gotProcessed != "1" {
			t.Errorf("GET %q = %q, want 1", processedKey, gotProcessed)
		}

		gotTTL := r.client.TTL(processedKey).Val()
		if gotTTL > statsTTL {
			t.Errorf("TTL %q = %v, want less than or equal to %v", processedKey, gotTTL, statsTTL)
		}

		if len(tc.target.UniqueKey) > 0 && r.client.Exists(tc.target.UniqueKey).Val() != 0 {
			t.Errorf("Uniqueness lock %q still exists", tc.target.UniqueKey)
		}
	}
}

func TestRequeue(t *testing.T) {
	r := setup(t)
	t1 := h.NewTaskMessage("send_email", nil)
	t2 := h.NewTaskMessage("export_csv", nil)
	t3 := h.NewTaskMessageWithQueue("send_email", nil, "critical")

	tests := []struct {
		enqueued       map[string][]*base.TaskMessage // initial state of queues
		inProgress     []*base.TaskMessage            // initial state of the in-progress list
		target         *base.TaskMessage              // task to requeue
		wantEnqueued   map[string][]*base.TaskMessage // final state of queues
		wantInProgress []*base.TaskMessage            // final state of the in-progress list
	}{
		{
			enqueued: map[string][]*base.TaskMessage{
				base.DefaultQueueName: {},
			},
			inProgress: []*base.TaskMessage{t1, t2},
			target:     t1,
			wantEnqueued: map[string][]*base.TaskMessage{
				base.DefaultQueueName: {t1},
			},
			wantInProgress: []*base.TaskMessage{t2},
		},
		{
			enqueued: map[string][]*base.TaskMessage{
				base.DefaultQueueName: {t1},
			},
			inProgress: []*base.TaskMessage{t2},
			target:     t2,
			wantEnqueued: map[string][]*base.TaskMessage{
				base.DefaultQueueName: {t1, t2},
			},
			wantInProgress: []*base.TaskMessage{},
		},
		{
			enqueued: map[string][]*base.TaskMessage{
				base.DefaultQueueName: {t1},
				"critical":            {},
			},
			inProgress: []*base.TaskMessage{t2, t3},
			target:     t3,
			wantEnqueued: map[string][]*base.TaskMessage{
				base.DefaultQueueName: {t1},
				"critical":            {t3},
			},
			wantInProgress: []*base.TaskMessage{t2},
		},
	}

	for _, tc := range tests {
		h.FlushDB(t, r.client) // clean up db before each test case
		for qname, msgs := range tc.enqueued {
			h.SeedEnqueuedQueue(t, r.client, msgs, qname)
		}
		h.SeedInProgressQueue(t, r.client, tc.inProgress)

		err := r.Requeue(tc.target)
		if err != nil {
			t.Errorf("(*RDB).Requeue(task) = %v, want nil", err)
			continue
		}

		for qname, want := range tc.wantEnqueued {
			gotEnqueued := h.GetEnqueuedMessages(t, r.client, qname)
			if diff := cmp.Diff(want, gotEnqueued, h.SortMsgOpt); diff != "" {
				t.Errorf("mismatch found in %q; (-want, +got)\n%s", base.QueueKey(qname), diff)
			}
		}

		gotInProgress := h.GetInProgressMessages(t, r.client)
		if diff := cmp.Diff(tc.wantInProgress, gotInProgress, h.SortMsgOpt); diff != "" {
			t.Errorf("mismatch found in %q: (-want, +got):\n%s", base.InProgressQueue, diff)
		}
	}
}

func TestSchedule(t *testing.T) {
	r := setup(t)
	t1 := h.NewTaskMessage("send_email", map[string]interface{}{"subject": "hello"})
	tests := []struct {
		msg       *base.TaskMessage
		processAt time.Time
	}{
		{t1, time.Now().Add(15 * time.Minute)},
	}

	for _, tc := range tests {
		h.FlushDB(t, r.client) // clean up db before each test case

		desc := fmt.Sprintf("(*RDB).Schedule(%v, %v)", tc.msg, tc.processAt)
		err := r.Schedule(tc.msg, tc.processAt)
		if err != nil {
			t.Errorf("%s = %v, want nil", desc, err)
			continue
		}

		gotScheduled := h.GetScheduledEntries(t, r.client)
		if len(gotScheduled) != 1 {
			t.Errorf("%s inserted %d items to %q, want 1 items inserted", desc, len(gotScheduled), base.ScheduledQueue)
			continue
		}
		if int64(gotScheduled[0].Score) != tc.processAt.Unix() {
			t.Errorf("%s inserted an item with score %d, want %d", desc, int64(gotScheduled[0].Score), tc.processAt.Unix())
			continue
		}
	}
}

func TestScheduleUnique(t *testing.T) {
	r := setup(t)
	m1 := base.TaskMessage{
		ID:        xid.New(),
		Type:      "email",
		Payload:   map[string]interface{}{"user_id": 123},
		Queue:     base.DefaultQueueName,
		UniqueKey: "email:user_id=123:default",
	}

	tests := []struct {
		msg       *base.TaskMessage
		processAt time.Time
		ttl       time.Duration // uniqueness lock ttl
	}{
		{&m1, time.Now().Add(15 * time.Minute), time.Minute},
	}

	for _, tc := range tests {
		h.FlushDB(t, r.client) // clean up db before each test case

		desc := fmt.Sprintf("(*RDB).ScheduleUnique(%v, %v, %v)", tc.msg, tc.processAt, tc.ttl)
		err := r.ScheduleUnique(tc.msg, tc.processAt, tc.ttl)
		if err != nil {
			t.Errorf("Frist task: %s = %v, want nil", desc, err)
			continue
		}

		gotScheduled := h.GetScheduledEntries(t, r.client)
		if len(gotScheduled) != 1 {
			t.Errorf("%s inserted %d items to %q, want 1 items inserted", desc, len(gotScheduled), base.ScheduledQueue)
			continue
		}
		if int64(gotScheduled[0].Score) != tc.processAt.Unix() {
			t.Errorf("%s inserted an item with score %d, want %d", desc, int64(gotScheduled[0].Score), tc.processAt.Unix())
			continue
		}

		got := r.ScheduleUnique(tc.msg, tc.processAt, tc.ttl)
		if got != ErrDuplicateTask {
			t.Errorf("Second task: %s = %v, want %v",
				desc, got, ErrDuplicateTask)
		}

		gotTTL := r.client.TTL(tc.msg.UniqueKey).Val()
		if !cmp.Equal(tc.ttl.Seconds(), gotTTL.Seconds(), cmpopts.EquateApprox(0, 1)) {
			t.Errorf("TTL %q = %v, want %v", tc.msg.UniqueKey, gotTTL, tc.ttl)
			continue
		}
	}
}

func TestRetry(t *testing.T) {
	r := setup(t)
	t1 := h.NewTaskMessage("send_email", map[string]interface{}{"subject": "Hola!"})
	t2 := h.NewTaskMessage("gen_thumbnail", map[string]interface{}{"path": "some/path/to/image.jpg"})
	t3 := h.NewTaskMessage("reindex", nil)
	t1.Retried = 10
	errMsg := "SMTP server is not responding"
	t1AfterRetry := &base.TaskMessage{
		ID:       t1.ID,
		Type:     t1.Type,
		Payload:  t1.Payload,
		Queue:    t1.Queue,
		Retry:    t1.Retry,
		Retried:  t1.Retried + 1,
		ErrorMsg: errMsg,
	}
	now := time.Now()

	tests := []struct {
		inProgress     []*base.TaskMessage
		retry          []h.ZSetEntry
		msg            *base.TaskMessage
		processAt      time.Time
		errMsg         string
		wantInProgress []*base.TaskMessage
		wantRetry      []h.ZSetEntry
	}{
		{
			inProgress: []*base.TaskMessage{t1, t2},
			retry: []h.ZSetEntry{
				{
					Msg:   t3,
					Score: float64(now.Add(time.Minute).Unix()),
				},
			},
			msg:            t1,
			processAt:      now.Add(5 * time.Minute),
			errMsg:         errMsg,
			wantInProgress: []*base.TaskMessage{t2},
			wantRetry: []h.ZSetEntry{
				{
					Msg:   t1AfterRetry,
					Score: float64(now.Add(5 * time.Minute).Unix()),
				},
				{
					Msg:   t3,
					Score: float64(now.Add(time.Minute).Unix()),
				},
			},
		},
	}

	for _, tc := range tests {
		h.FlushDB(t, r.client)
		h.SeedInProgressQueue(t, r.client, tc.inProgress)
		h.SeedRetryQueue(t, r.client, tc.retry)

		err := r.Retry(tc.msg, tc.processAt, tc.errMsg)
		if err != nil {
			t.Errorf("(*RDB).Retry = %v, want nil", err)
			continue
		}

		gotInProgress := h.GetInProgressMessages(t, r.client)
		if diff := cmp.Diff(tc.wantInProgress, gotInProgress, h.SortMsgOpt); diff != "" {
			t.Errorf("mismatch found in %q; (-want, +got)\n%s", base.InProgressQueue, diff)
		}

		gotRetry := h.GetRetryEntries(t, r.client)
		if diff := cmp.Diff(tc.wantRetry, gotRetry, h.SortZSetEntryOpt); diff != "" {
			t.Errorf("mismatch found in %q; (-want, +got)\n%s", base.RetryQueue, diff)
		}

		processedKey := base.ProcessedKey(time.Now())
		gotProcessed := r.client.Get(processedKey).Val()
		if gotProcessed != "1" {
			t.Errorf("GET %q = %q, want 1", processedKey, gotProcessed)
		}
		gotTTL := r.client.TTL(processedKey).Val()
		if gotTTL > statsTTL {
			t.Errorf("TTL %q = %v, want less than or equal to %v", processedKey, gotTTL, statsTTL)
		}

		failureKey := base.FailureKey(time.Now())
		gotFailure := r.client.Get(failureKey).Val()
		if gotFailure != "1" {
			t.Errorf("GET %q = %q, want 1", failureKey, gotFailure)
		}
		gotTTL = r.client.TTL(processedKey).Val()
		if gotTTL > statsTTL {
			t.Errorf("TTL %q = %v, want less than or equal to %v", failureKey, gotTTL, statsTTL)
		}
	}
}

func TestKill(t *testing.T) {
	r := setup(t)
	t1 := h.NewTaskMessage("send_email", nil)
	t2 := h.NewTaskMessage("reindex", nil)
	t3 := h.NewTaskMessage("generate_csv", nil)
	errMsg := "SMTP server not responding"
	t1AfterKill := &base.TaskMessage{
		ID:       t1.ID,
		Type:     t1.Type,
		Payload:  t1.Payload,
		Queue:    t1.Queue,
		Retry:    t1.Retry,
		Retried:  t1.Retried,
		ErrorMsg: errMsg,
	}
	now := time.Now()

	// TODO(hibiken): add test cases for trimming
	tests := []struct {
		inProgress     []*base.TaskMessage
		dead           []h.ZSetEntry
		target         *base.TaskMessage // task to kill
		wantInProgress []*base.TaskMessage
		wantDead       []h.ZSetEntry
	}{
		{
			inProgress: []*base.TaskMessage{t1, t2},
			dead: []h.ZSetEntry{
				{
					Msg:   t3,
					Score: float64(now.Add(-time.Hour).Unix()),
				},
			},
			target:         t1,
			wantInProgress: []*base.TaskMessage{t2},
			wantDead: []h.ZSetEntry{
				{
					Msg:   t1AfterKill,
					Score: float64(now.Unix()),
				},
				{
					Msg:   t3,
					Score: float64(now.Add(-time.Hour).Unix()),
				},
			},
		},
		{
			inProgress:     []*base.TaskMessage{t1, t2, t3},
			dead:           []h.ZSetEntry{},
			target:         t1,
			wantInProgress: []*base.TaskMessage{t2, t3},
			wantDead: []h.ZSetEntry{
				{
					Msg:   t1AfterKill,
					Score: float64(now.Unix()),
				},
			},
		},
	}

	for _, tc := range tests {
		h.FlushDB(t, r.client) // clean up db before each test case
		h.SeedInProgressQueue(t, r.client, tc.inProgress)
		h.SeedDeadQueue(t, r.client, tc.dead)

		err := r.Kill(tc.target, errMsg)
		if err != nil {
			t.Errorf("(*RDB).Kill(%v, %v) = %v, want nil", tc.target, errMsg, err)
			continue
		}

		gotInProgress := h.GetInProgressMessages(t, r.client)
		if diff := cmp.Diff(tc.wantInProgress, gotInProgress, h.SortMsgOpt); diff != "" {
			t.Errorf("mismatch found in %q: (-want, +got)\n%s", base.InProgressQueue, diff)
		}

		gotDead := h.GetDeadEntries(t, r.client)
		if diff := cmp.Diff(tc.wantDead, gotDead, h.SortZSetEntryOpt); diff != "" {
			t.Errorf("mismatch found in %q after calling (*RDB).Kill: (-want, +got):\n%s", base.DeadQueue, diff)
		}

		processedKey := base.ProcessedKey(time.Now())
		gotProcessed := r.client.Get(processedKey).Val()
		if gotProcessed != "1" {
			t.Errorf("GET %q = %q, want 1", processedKey, gotProcessed)
		}
		gotTTL := r.client.TTL(processedKey).Val()
		if gotTTL > statsTTL {
			t.Errorf("TTL %q = %v, want less than or equal to %v", processedKey, gotTTL, statsTTL)
		}

		failureKey := base.FailureKey(time.Now())
		gotFailure := r.client.Get(failureKey).Val()
		if gotFailure != "1" {
			t.Errorf("GET %q = %q, want 1", failureKey, gotFailure)
		}
		gotTTL = r.client.TTL(processedKey).Val()
		if gotTTL > statsTTL {
			t.Errorf("TTL %q = %v, want less than or equal to %v", failureKey, gotTTL, statsTTL)
		}
	}
}

func TestRequeueAll(t *testing.T) {
	r := setup(t)
	t1 := h.NewTaskMessage("send_email", nil)
	t2 := h.NewTaskMessage("export_csv", nil)
	t3 := h.NewTaskMessage("sync_stuff", nil)
	t4 := h.NewTaskMessageWithQueue("important", nil, "critical")
	t5 := h.NewTaskMessageWithQueue("minor", nil, "low")

	tests := []struct {
		inProgress     []*base.TaskMessage
		enqueued       map[string][]*base.TaskMessage
		want           int64
		wantInProgress []*base.TaskMessage
		wantEnqueued   map[string][]*base.TaskMessage
	}{
		{
			inProgress: []*base.TaskMessage{t1, t2, t3},
			enqueued: map[string][]*base.TaskMessage{
				base.DefaultQueueName: {},
			},
			want:           3,
			wantInProgress: []*base.TaskMessage{},
			wantEnqueued: map[string][]*base.TaskMessage{
				base.DefaultQueueName: {t1, t2, t3},
			},
		},
		{
			inProgress: []*base.TaskMessage{},
			enqueued: map[string][]*base.TaskMessage{
				base.DefaultQueueName: {t1, t2, t3},
			},
			want:           0,
			wantInProgress: []*base.TaskMessage{},
			wantEnqueued: map[string][]*base.TaskMessage{
				base.DefaultQueueName: {t1, t2, t3},
			},
		},
		{
			inProgress: []*base.TaskMessage{t2, t3},
			enqueued: map[string][]*base.TaskMessage{
				base.DefaultQueueName: {t1},
			},
			want:           2,
			wantInProgress: []*base.TaskMessage{},
			wantEnqueued: map[string][]*base.TaskMessage{
				base.DefaultQueueName: {t1, t2, t3},
			},
		},
		{
			inProgress: []*base.TaskMessage{t2, t3, t4, t5},
			enqueued: map[string][]*base.TaskMessage{
				base.DefaultQueueName: {t1},
				"critical":            {},
				"low":                 {},
			},
			want:           4,
			wantInProgress: []*base.TaskMessage{},
			wantEnqueued: map[string][]*base.TaskMessage{
				base.DefaultQueueName: {t1, t2, t3},
				"critical":            {t4},
				"low":                 {t5},
			},
		},
	}

	for _, tc := range tests {
		h.FlushDB(t, r.client) // clean up db before each test case
		h.SeedInProgressQueue(t, r.client, tc.inProgress)
		for qname, msgs := range tc.enqueued {
			h.SeedEnqueuedQueue(t, r.client, msgs, qname)
		}

		got, err := r.RequeueAll()
		if got != tc.want || err != nil {
			t.Errorf("(*RDB).RequeueAll() = %v %v, want %v nil", got, err, tc.want)
			continue
		}

		gotInProgress := h.GetInProgressMessages(t, r.client)
		if diff := cmp.Diff(tc.wantInProgress, gotInProgress, h.SortMsgOpt); diff != "" {
			t.Errorf("mismatch found in %q: (-want, +got):\n%s", base.InProgressQueue, diff)
		}

		for qname, want := range tc.wantEnqueued {
			gotEnqueued := h.GetEnqueuedMessages(t, r.client, qname)
			if diff := cmp.Diff(want, gotEnqueued, h.SortMsgOpt); diff != "" {
				t.Errorf("mismatch found in %q: (-want, +got):\n%s", base.QueueKey(qname), diff)
			}
		}
	}
}

func TestCheckAndEnqueue(t *testing.T) {
	r := setup(t)
	t1 := h.NewTaskMessage("send_email", nil)
	t2 := h.NewTaskMessage("generate_csv", nil)
	t3 := h.NewTaskMessage("gen_thumbnail", nil)
	t4 := h.NewTaskMessage("important_task", nil)
	t4.Queue = "critical"
	t5 := h.NewTaskMessage("minor_task", nil)
	t5.Queue = "low"
	secondAgo := time.Now().Add(-time.Second)
	hourFromNow := time.Now().Add(time.Hour)

	tests := []struct {
		scheduled     []h.ZSetEntry
		retry         []h.ZSetEntry
		qnames        []string
		wantEnqueued  map[string][]*base.TaskMessage
		wantScheduled []*base.TaskMessage
		wantRetry     []*base.TaskMessage
	}{
		{
			scheduled: []h.ZSetEntry{
				{Msg: t1, Score: float64(secondAgo.Unix())},
				{Msg: t2, Score: float64(secondAgo.Unix())},
			},
			retry: []h.ZSetEntry{
				{Msg: t3, Score: float64(secondAgo.Unix())}},
			qnames: []string{"default"},
			wantEnqueued: map[string][]*base.TaskMessage{
				"default": {t1, t2, t3},
			},
			wantScheduled: []*base.TaskMessage{},
			wantRetry:     []*base.TaskMessage{},
		},
		{
			scheduled: []h.ZSetEntry{
				{Msg: t1, Score: float64(hourFromNow.Unix())},
				{Msg: t2, Score: float64(secondAgo.Unix())}},
			retry: []h.ZSetEntry{
				{Msg: t3, Score: float64(secondAgo.Unix())}},
			qnames: []string{"default"},
			wantEnqueued: map[string][]*base.TaskMessage{
				"default": {t2, t3},
			},
			wantScheduled: []*base.TaskMessage{t1},
			wantRetry:     []*base.TaskMessage{},
		},
		{
			scheduled: []h.ZSetEntry{
				{Msg: t1, Score: float64(hourFromNow.Unix())},
				{Msg: t2, Score: float64(hourFromNow.Unix())}},
			retry: []h.ZSetEntry{
				{Msg: t3, Score: float64(hourFromNow.Unix())}},
			qnames: []string{"default"},
			wantEnqueued: map[string][]*base.TaskMessage{
				"default": {},
			},
			wantScheduled: []*base.TaskMessage{t1, t2},
			wantRetry:     []*base.TaskMessage{t3},
		},
		{
			scheduled: []h.ZSetEntry{
				{Msg: t1, Score: float64(secondAgo.Unix())},
				{Msg: t4, Score: float64(secondAgo.Unix())},
			},
			retry: []h.ZSetEntry{
				{Msg: t5, Score: float64(secondAgo.Unix())}},
			qnames: []string{"default", "critical", "low"},
			wantEnqueued: map[string][]*base.TaskMessage{
				"default":  {t1},
				"critical": {t4},
				"low":      {t5},
			},
			wantScheduled: []*base.TaskMessage{},
			wantRetry:     []*base.TaskMessage{},
		},
	}

	for _, tc := range tests {
		h.FlushDB(t, r.client) // clean up db before each test case
		h.SeedScheduledQueue(t, r.client, tc.scheduled)
		h.SeedRetryQueue(t, r.client, tc.retry)

		err := r.CheckAndEnqueue(tc.qnames...)
		if err != nil {
			t.Errorf("(*RDB).CheckScheduled() = %v, want nil", err)
			continue
		}

		for qname, want := range tc.wantEnqueued {
			gotEnqueued := h.GetEnqueuedMessages(t, r.client, qname)
			if diff := cmp.Diff(want, gotEnqueued, h.SortMsgOpt); diff != "" {
				t.Errorf("mismatch found in %q; (-want, +got)\n%s", base.QueueKey(qname), diff)
			}
		}

		gotScheduled := h.GetScheduledMessages(t, r.client)
		if diff := cmp.Diff(tc.wantScheduled, gotScheduled, h.SortMsgOpt); diff != "" {
			t.Errorf("mismatch found in %q; (-want, +got)\n%s", base.ScheduledQueue, diff)
		}

		gotRetry := h.GetRetryMessages(t, r.client)
		if diff := cmp.Diff(tc.wantRetry, gotRetry, h.SortMsgOpt); diff != "" {
			t.Errorf("mismatch found in %q; (-want, +got)\n%s", base.RetryQueue, diff)
		}
	}
}

func TestWriteServerState(t *testing.T) {
	r := setup(t)
	queues := map[string]int{"default": 2, "email": 5, "low": 1}

	started := time.Now()
	ss := base.NewServerState("localhost", 4242, 10, queues, false)
	ss.SetStarted(started)
	ss.SetStatus(base.StatusRunning)
	ttl := 5 * time.Second

	h.FlushDB(t, r.client)

	err := r.WriteServerState(ss, ttl)
	if err != nil {
		t.Errorf("r.WriteServerState returned an error: %v", err)
	}

	// Check ServerInfo was written correctly
	info := ss.GetInfo()
	skey := base.ServerInfoKey(info.Host, info.PID, info.ServerID)
	data := r.client.Get(skey).Val()
	var got base.ServerInfo
	err = json.Unmarshal([]byte(data), &got)
	if err != nil {
		t.Fatalf("could not decode json: %v", err)
	}
	want := base.ServerInfo{
		Host:              info.Host,
		PID:               info.PID,
		Concurrency:       info.Concurrency,
		Queues:            map[string]int{"default": 2, "email": 5, "low": 1},
		StrictPriority:    false,
		Status:            "running",
		Started:           started,
		ActiveWorkerCount: 0,
	}
	ignoreOpt := cmpopts.IgnoreFields(base.ServerInfo{}, "ServerID")
	if diff := cmp.Diff(want, got, ignoreOpt); diff != "" {
		t.Errorf("persisted ServerInfo was %v, want %v; (-want,+got)\n%s",
			got, want, diff)
	}
	// Check ServerInfo TTL was set correctly
	gotTTL := r.client.TTL(skey).Val()
	if !cmp.Equal(ttl.Seconds(), gotTTL.Seconds(), cmpopts.EquateApprox(0, 1)) {
		t.Errorf("TTL of %q was %v, want %v", skey, gotTTL, ttl)
	}
	// Check ServerInfo key was added to the set correctly
	gotProcesses := r.client.ZRange(base.AllServers, 0, -1).Val()
	wantProcesses := []string{skey}
	if diff := cmp.Diff(wantProcesses, gotProcesses); diff != "" {
		t.Errorf("%q contained %v, want %v", base.AllServers, gotProcesses, wantProcesses)
	}

	// Check WorkersInfo was written correctly
	wkey := base.WorkersKey(info.Host, info.PID, info.ServerID)
	workerExist := r.client.Exists(wkey).Val()
	if workerExist != 0 {
		t.Errorf("%q key exists", wkey)
	}
	// Check WorkersInfo key was added to the set correctly
	gotWorkerKeys := r.client.ZRange(base.AllWorkers, 0, -1).Val()
	wantWorkerKeys := []string{wkey}
	if diff := cmp.Diff(wantWorkerKeys, gotWorkerKeys); diff != "" {
		t.Errorf("%q contained %v, want %v", base.AllWorkers, gotWorkerKeys, wantWorkerKeys)
	}
}

func TestWriteServerStateWithWorkers(t *testing.T) {
	r := setup(t)
	queues := map[string]int{"default": 2, "email": 5, "low": 1}
	concurrency := 10

	started := time.Now().Add(-10 * time.Minute)
	w1Started := time.Now().Add(-time.Minute)
	w2Started := time.Now().Add(-time.Second)
	msg1 := h.NewTaskMessage("send_email", map[string]interface{}{"user_id": "123"})
	msg2 := h.NewTaskMessage("gen_thumbnail", map[string]interface{}{"path": "some/path/to/imgfile"})
	ss := base.NewServerState("127.0.01", 4242, concurrency, queues, false)
	ss.SetStarted(started)
	ss.SetStatus(base.StatusRunning)
	ss.AddWorkerStats(msg1, w1Started)
	ss.AddWorkerStats(msg2, w2Started)
	ttl := 5 * time.Second

	h.FlushDB(t, r.client)

	err := r.WriteServerState(ss, ttl)
	if err != nil {
		t.Errorf("r.WriteServerState returned an error: %v", err)
	}

	// Check ServerInfo was written correctly
	info := ss.GetInfo()
	skey := base.ServerInfoKey(info.Host, info.PID, info.ServerID)
	data := r.client.Get(skey).Val()
	var got base.ServerInfo
	err = json.Unmarshal([]byte(data), &got)
	if err != nil {
		t.Fatalf("could not decode json: %v", err)
	}
	want := base.ServerInfo{
		Host:              info.Host,
		PID:               info.PID,
		ServerID:          info.ServerID,
		Concurrency:       concurrency,
		Queues:            queues,
		StrictPriority:    false,
		Status:            "running",
		Started:           started,
		ActiveWorkerCount: 2,
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("persisted ServerInfo was %v, want %v; (-want,+got)\n%s",
			got, want, diff)
	}
	// Check ServerInfo TTL was set correctly
	gotTTL := r.client.TTL(skey).Val()
	if !cmp.Equal(ttl.Seconds(), gotTTL.Seconds(), cmpopts.EquateApprox(0, 1)) {
		t.Errorf("TTL of %q was %v, want %v", skey, gotTTL, ttl)
	}
	// Check ServerInfo key was added to the set correctly
	gotProcesses := r.client.ZRange(base.AllServers, 0, -1).Val()
	wantProcesses := []string{skey}
	if diff := cmp.Diff(wantProcesses, gotProcesses); diff != "" {
		t.Errorf("%q contained %v, want %v", base.AllServers, gotProcesses, wantProcesses)
	}

	// Check WorkersInfo was written correctly
	wkey := base.WorkersKey(info.Host, info.PID, info.ServerID)
	wdata := r.client.HGetAll(wkey).Val()
	if len(wdata) != 2 {
		t.Fatalf("HGETALL %q returned a hash of size %d, want 2", wkey, len(wdata))
	}
	gotWorkers := make(map[string]*base.WorkerInfo)
	for key, val := range wdata {
		var w base.WorkerInfo
		if err := json.Unmarshal([]byte(val), &w); err != nil {
			t.Fatalf("could not unmarshal worker's data: %v", err)
		}
		gotWorkers[key] = &w
	}
	wantWorkers := map[string]*base.WorkerInfo{
		msg1.ID.String(): {
			Host:    info.Host,
			PID:     info.PID,
			ID:      msg1.ID,
			Type:    msg1.Type,
			Queue:   msg1.Queue,
			Payload: msg1.Payload,
			Started: w1Started,
		},
		msg2.ID.String(): {
			Host:    info.Host,
			PID:     info.PID,
			ID:      msg2.ID,
			Type:    msg2.Type,
			Queue:   msg2.Queue,
			Payload: msg2.Payload,
			Started: w2Started,
		},
	}
	if diff := cmp.Diff(wantWorkers, gotWorkers); diff != "" {
		t.Errorf("persisted workers info was %v, want %v; (-want,+got)\n%s",
			gotWorkers, wantWorkers, diff)
	}

	// Check WorkersInfo TTL was set correctly
	gotTTL = r.client.TTL(wkey).Val()
	if !cmp.Equal(ttl, gotTTL, timeCmpOpt) {
		t.Errorf("TTL of %q was %v, want %v", wkey, gotTTL, ttl)
	}
	// Check WorkersInfo key was added to the set correctly
	gotWorkerKeys := r.client.ZRange(base.AllWorkers, 0, -1).Val()
	wantWorkerKeys := []string{wkey}
	if diff := cmp.Diff(wantWorkerKeys, gotWorkerKeys); diff != "" {
		t.Errorf("%q contained %v, want %v", base.AllWorkers, gotWorkerKeys, wantWorkerKeys)
	}
}

func TestClearServerState(t *testing.T) {
	r := setup(t)
	ss := base.NewServerState("127.0.01", 4242, 10, map[string]int{"default": 1}, false)
	info := ss.GetInfo()

	h.FlushDB(t, r.client)

	skey := base.ServerInfoKey(info.Host, info.PID, info.ServerID)
	wkey := base.WorkersKey(info.Host, info.PID, info.ServerID)
	otherSKey := base.ServerInfoKey("otherhost", 12345, "server98")
	otherWKey := base.WorkersKey("otherhost", 12345, "server98")
	// Populate the keys.
	if err := r.client.Set(skey, "process-info", 0).Err(); err != nil {
		t.Fatal(err)
	}
	if err := r.client.HSet(wkey, "worker-key", "worker-info").Err(); err != nil {
		t.Fatal(err)
	}
	if err := r.client.ZAdd(base.AllServers, &redis.Z{Member: skey}).Err(); err != nil {
		t.Fatal(err)
	}
	if err := r.client.ZAdd(base.AllServers, &redis.Z{Member: otherSKey}).Err(); err != nil {
		t.Fatal(err)
	}
	if err := r.client.ZAdd(base.AllWorkers, &redis.Z{Member: wkey}).Err(); err != nil {
		t.Fatal(err)
	}
	if err := r.client.ZAdd(base.AllWorkers, &redis.Z{Member: otherWKey}).Err(); err != nil {
		t.Fatal(err)
	}

	err := r.ClearServerState(ss)
	if err != nil {
		t.Fatalf("(*RDB).ClearServerState failed: %v", err)
	}

	// Check all keys are cleared
	if r.client.Exists(skey).Val() != 0 {
		t.Errorf("Redis key %q exists", skey)
	}
	if r.client.Exists(wkey).Val() != 0 {
		t.Errorf("Redis key %q exists", wkey)
	}
	gotProcessKeys := r.client.ZRange(base.AllServers, 0, -1).Val()
	wantProcessKeys := []string{otherSKey}
	if diff := cmp.Diff(wantProcessKeys, gotProcessKeys); diff != "" {
		t.Errorf("%q contained %v, want %v", base.AllServers, gotProcessKeys, wantProcessKeys)
	}
	gotWorkerKeys := r.client.ZRange(base.AllWorkers, 0, -1).Val()
	wantWorkerKeys := []string{otherWKey}
	if diff := cmp.Diff(wantWorkerKeys, gotWorkerKeys); diff != "" {
		t.Errorf("%q contained %v, want %v", base.AllWorkers, gotWorkerKeys, wantWorkerKeys)
	}
}

func TestCancelationPubSub(t *testing.T) {
	r := setup(t)

	pubsub, err := r.CancelationPubSub()
	if err != nil {
		t.Fatalf("(*RDB).CancelationPubSub() returned an error: %v", err)
	}

	cancelCh := pubsub.Channel()

	var (
		mu       sync.Mutex
		received []string
	)

	go func() {
		for msg := range cancelCh {
			mu.Lock()
			received = append(received, msg.Payload)
			mu.Unlock()
		}
	}()

	publish := []string{"one", "two", "three"}

	for _, msg := range publish {
		r.PublishCancelation(msg)
	}

	// allow for message to reach subscribers.
	time.Sleep(time.Second)

	pubsub.Close()

	mu.Lock()
	if diff := cmp.Diff(publish, received, h.SortStringSliceOpt); diff != "" {
		t.Errorf("subscriber received %v, want %v; (-want,+got)\n%s", received, publish, diff)
	}
	mu.Unlock()
}
