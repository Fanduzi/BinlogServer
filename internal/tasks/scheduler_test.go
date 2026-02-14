package tasks

import "testing"

func TestScheduler_StartTaskTransitionsToRunning(t *testing.T) {
	s := NewScheduler()
	task, err := s.CreateTask("cluster-a")
	if err != nil {
		t.Fatalf("CreateTask returned error: %v", err)
	}

	if err := s.StartTask(task.ID); err != nil {
		t.Fatalf("StartTask returned error: %v", err)
	}

	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if got.State != StateRunning {
		t.Fatalf("expected state %s, got %s", StateRunning, got.State)
	}
}

func TestScheduler_RetryableErrorTransitionsToBackoff(t *testing.T) {
	s := NewScheduler()
	task, err := s.CreateTask("cluster-a")
	if err != nil {
		t.Fatalf("CreateTask returned error: %v", err)
	}
	if err := s.StartTask(task.ID); err != nil {
		t.Fatalf("StartTask returned error: %v", err)
	}

	if err := s.MarkRetryableError(task.ID, "network timeout"); err != nil {
		t.Fatalf("MarkRetryableError returned error: %v", err)
	}

	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if got.State != StateRetryBackoff {
		t.Fatalf("expected state %s, got %s", StateRetryBackoff, got.State)
	}
	if got.LastError != "network timeout" {
		t.Fatalf("expected last error to be recorded, got %q", got.LastError)
	}
}

func TestScheduler_StopTaskTransitionsToStopped(t *testing.T) {
	s := NewScheduler()
	task, err := s.CreateTask("cluster-a")
	if err != nil {
		t.Fatalf("CreateTask returned error: %v", err)
	}
	if err := s.StartTask(task.ID); err != nil {
		t.Fatalf("StartTask returned error: %v", err)
	}

	if err := s.StopTask(task.ID); err != nil {
		t.Fatalf("StopTask returned error: %v", err)
	}

	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if got.State != StateStopped {
		t.Fatalf("expected state %s, got %s", StateStopped, got.State)
	}
}
