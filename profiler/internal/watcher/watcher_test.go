package watcher

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestComputePodDuration_WithStartTime(t *testing.T) {
	now := time.Now()
	pod := &corev1.Pod{
		Status: corev1.PodStatus{
			StartTime: &metav1.Time{Time: now.Add(-10 * time.Minute)},
			ContainerStatuses: []corev1.ContainerStatus{
				{
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							FinishedAt: metav1.Time{Time: now},
						},
					},
				},
			},
		},
	}

	start, end, dur := computePodDuration(pod)

	if !start.Equal(now.Add(-10 * time.Minute)) {
		t.Errorf("start = %v, want %v", start, now.Add(-10*time.Minute))
	}
	if !end.Equal(now) {
		t.Errorf("end = %v, want %v", end, now)
	}
	// Duration should be ~10 minutes
	if dur < 9*time.Minute || dur > 11*time.Minute {
		t.Errorf("duration = %s, want ~10m", dur)
	}
}

func TestComputePodDuration_NoTermination(t *testing.T) {
	now := time.Now()
	pod := &corev1.Pod{
		Status: corev1.PodStatus{
			StartTime: &metav1.Time{Time: now.Add(-5 * time.Minute)},
		},
	}

	_, end, dur := computePodDuration(pod)

	// end should be approximately now
	if time.Since(end) > 2*time.Second {
		t.Errorf("end should be ~now, got %v", end)
	}
	if dur < 4*time.Minute || dur > 6*time.Minute {
		t.Errorf("duration = %s, want ~5m", dur)
	}
}

func TestComputePodDuration_MinimumDuration(t *testing.T) {
	now := time.Now()
	pod := &corev1.Pod{
		Status: corev1.PodStatus{
			StartTime: &metav1.Time{Time: now},
			ContainerStatuses: []corev1.ContainerStatus{
				{
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							FinishedAt: metav1.Time{Time: now},
						},
					},
				},
			},
		},
	}

	_, _, dur := computePodDuration(pod)

	// Minimum 1 second
	if dur < time.Second {
		t.Errorf("duration = %s, want >= 1s", dur)
	}
}

func TestComputePodDuration_FallbackToCreationTimestamp(t *testing.T) {
	now := time.Now()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			CreationTimestamp: metav1.Time{Time: now.Add(-3 * time.Minute)},
		},
		Status: corev1.PodStatus{
			// No StartTime
			ContainerStatuses: []corev1.ContainerStatus{
				{
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							FinishedAt: metav1.Time{Time: now},
						},
					},
				},
			},
		},
	}

	start, _, _ := computePodDuration(pod)

	if !start.Equal(now.Add(-3 * time.Minute)) {
		t.Errorf("start should fall back to CreationTimestamp, got %v", start)
	}
}

func TestPodCompletionEvent_Fields(t *testing.T) {
	now := time.Now()
	evt := PodCompletionEvent{
		PodName:      "my-pod",
		PodNamespace: "my-ns",
		Phase:        corev1.PodSucceeded,
		StartTime:    now.Add(-1 * time.Hour),
		EndTime:      now,
		Duration:     1 * time.Hour,
	}

	if evt.PodName != "my-pod" {
		t.Errorf("PodName = %q", evt.PodName)
	}
	if evt.Phase != corev1.PodSucceeded {
		t.Errorf("Phase = %v", evt.Phase)
	}
}

func TestPreRunEvent_Fields(t *testing.T) {
	now := time.Now()
	evt := PreRunEvent{
		PodName:      "profiling-pod",
		PodNamespace: "test",
		StartTime:    now,
	}

	if evt.PodName != "profiling-pod" {
		t.Errorf("PodName = %q", evt.PodName)
	}
	if evt.PodNamespace != "test" {
		t.Errorf("PodNamespace = %q", evt.PodNamespace)
	}
}
