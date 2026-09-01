package maclet

import (
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestParseCPUTime(t *testing.T) {
	tests := []struct {
		value string
		want  float64
	}{
		{value: "00:01.25", want: 1.25},
		{value: "01:02:03.50", want: 3723.5},
		{value: "2-03:04:05.75", want: 183845.75},
	}
	for _, test := range tests {
		got, err := parseCPUTime(test.value)
		if err != nil {
			t.Errorf("parseCPUTime(%q): %v", test.value, err)
			continue
		}
		if got != test.want {
			t.Errorf("parseCPUTime(%q) = %v, want %v", test.value, got, test.want)
		}
	}
}

func TestProcessTreeMetrics(t *testing.T) {
	processes := map[int]processMetric{
		10: {pid: 10, parent: 1, cpuSeconds: 2, memoryBytes: 100},
		11: {pid: 11, parent: 10, cpuSeconds: 3, memoryBytes: 200},
		12: {pid: 12, parent: 11, cpuSeconds: 5, memoryBytes: 300},
		20: {pid: 20, parent: 1, cpuSeconds: 100, memoryBytes: 1000},
	}
	cpu, memory, found := processTreeMetrics(processes, 10)
	if !found || cpu != 10 || memory != 600 {
		t.Fatalf("processTreeMetrics = %v, %d, %v", cpu, memory, found)
	}
	if _, _, found := processTreeMetrics(processes, 99); found {
		t.Fatal("missing process was found")
	}
}

func TestMetricCounterIsMonotonicAcrossProcessExit(t *testing.T) {
	var counter metricCounter
	if got := updateMetricCounter(&counter, 10, 42); got != 10 {
		t.Fatalf("initial counter = %v", got)
	}
	if got := updateMetricCounter(&counter, 12, 42); got != 12 {
		t.Fatalf("counter after increase = %v", got)
	}
	if got := updateMetricCounter(&counter, 4, 42); got != 12 {
		t.Fatalf("counter after process exit = %v", got)
	}
	if got := updateMetricCounter(&counter, 5, 42); got != 13 {
		t.Fatalf("counter after new work = %v", got)
	}
	if got := updateMetricCounter(&counter, 2, 43); got != 15 {
		t.Fatalf("counter after process restart = %v", got)
	}
}

func TestFormatResourceMetrics(t *testing.T) {
	start := time.Unix(1700000000, 0).UTC()
	body := formatResourceMetrics(start, 12.5, 1024, []workloadMetric{{
		workload:    managedWorkload{Namespace: "default", Name: "web", PodContainerName: "app"},
		cpuSeconds:  2.5,
		memoryBytes: 2048,
		startTime:   start,
	}}, 0)
	for _, expected := range []string{
		"node_cpu_usage_seconds_total 12.5 1700000000000",
		"node_memory_working_set_bytes 1024 1700000000000",
		`container_cpu_usage_seconds_total{container="app",pod="web",namespace="default"} 2.5 1700000000000`,
		`container_memory_working_set_bytes{container="app",pod="web",namespace="default"} 2048 1700000000000`,
		`pod_cpu_usage_seconds_total{pod="web",namespace="default"} 2.5 1700000000000`,
		"resource_scrape_error 0",
		"# EOF",
	} {
		if !strings.Contains(body, expected) {
			t.Errorf("metrics missing %q:\n%s", expected, body)
		}
	}
}

func TestKubeletResourceMetricsPathUsesBearerAuthentication(t *testing.T) {
	handler := &kubeletHandler{}
	request := httptest.NewRequest(http.MethodGet, kubeletResourceMetricsPath, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want %d", response.Code, http.StatusUnauthorized)
	}

	request = httptest.NewRequest(http.MethodGet, kubeletResourceMetricsPath, nil)
	request.Header.Set("Authorization", "Bearer metrics-server-token")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("bearer-authenticated status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
}

func TestReadNodeWorkingSetOnDarwin(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Darwin host statistics")
	}
	memory, err := readNodeWorkingSet()
	if err != nil {
		t.Fatal(err)
	}
	if memory == 0 {
		t.Fatal("host working set is zero")
	}
}
