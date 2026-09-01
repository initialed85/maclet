package maclet

import (
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

const kubeletResourceMetricsPath = "/metrics/resource"

type processMetric struct {
	pid         int
	parent      int
	cpuSeconds  float64
	memoryBytes uint64
}

type processMetricSnapshot struct {
	processes map[int]processMetric
}

type metricCounter struct {
	initialized bool
	observed    float64
	value       float64
	rootPID     int
}

type resourceMetrics struct {
	mu           sync.Mutex
	nodeCPU      metricCounter
	containerCPU map[string]metricCounter
}

type workloadMetric struct {
	workload    managedWorkload
	inspection  *mackerInspection
	cpuSeconds  float64
	memoryBytes uint64
	startTime   time.Time
}

func newResourceMetrics() *resourceMetrics {
	return &resourceMetrics{containerCPU: make(map[string]metricCounter)}
}

func (m *resourceMetrics) collect(manager *workloadManager) (string, error) {
	now := time.Now().UTC()
	processes, err := readProcessMetrics()
	if err != nil {
		return "", err
	}
	nodeCPU := 0.0
	for _, process := range processes.processes {
		nodeCPU += process.cpuSeconds
	}

	m.mu.Lock()
	nodeCPUValue := updateMetricCounter(&m.nodeCPU, nodeCPU, 0)
	m.mu.Unlock()

	var workloads []workloadMetric
	var scrapeErrors int
	if manager != nil {
		for _, workload := range manager.managedWorkloads() {
			inspection, available, inspectErr := manager.containerInspection(workload.ContainerName)
			if inspectErr != nil {
				scrapeErrors++
				continue
			}
			if !available || inspection == nil || inspection.Status != "running" {
				continue
			}
			rootPID := inspection.WorkloadPID
			if rootPID <= 0 {
				// Macker publishes the native child as WorkloadPID after it
				// exits. While it is running, PID is the detached launcher;
				// its process tree contains the native workload.
				rootPID = inspection.PID
			}
			if rootPID <= 0 {
				scrapeErrors++
				continue
			}
			cpuSeconds, memoryBytes, found := processTreeMetrics(processes.processes, rootPID)
			if !found {
				scrapeErrors++
				continue
			}
			startTime := now
			if inspection.StartedAt != nil && !inspection.StartedAt.IsZero() {
				startTime = inspection.StartedAt.UTC()
			}
			m.mu.Lock()
			containerCPU := m.containerCPU[workload.ContainerName]
			containerCPUValue := updateMetricCounter(&containerCPU, cpuSeconds, rootPID)
			m.containerCPU[workload.ContainerName] = containerCPU
			m.mu.Unlock()
			workloads = append(workloads, workloadMetric{
				workload:    workload,
				inspection:  inspection,
				cpuSeconds:  containerCPUValue,
				memoryBytes: memoryBytes,
				startTime:   startTime,
			})
		}
	}

	memoryBytes, memoryErr := readNodeWorkingSet()
	if memoryErr != nil {
		return "", memoryErr
	}
	return formatResourceMetrics(now, nodeCPUValue, memoryBytes, workloads, scrapeErrors), nil
}

func updateMetricCounter(counter *metricCounter, current float64, rootPID int) float64 {
	if !counter.initialized {
		counter.value = current
		counter.initialized = true
	} else if counter.rootPID != 0 && rootPID != 0 && counter.rootPID != rootPID {
		counter.value += current
	} else if current >= counter.observed {
		counter.value += current - counter.observed
	}
	counter.observed = current
	if rootPID != 0 {
		counter.rootPID = rootPID
	}
	return counter.value
}

func (m *workloadManager) managedWorkloads() []managedWorkload {
	m.mu.RLock()
	defer m.mu.RUnlock()
	workloads := make([]managedWorkload, 0, len(m.workloads))
	for _, workload := range m.workloads {
		if workload != nil {
			workloads = append(workloads, *workload)
		}
	}
	return workloads
}

func readProcessMetrics() (processMetricSnapshot, error) {
	output, err := exec.Command("ps", "-axo", "pid=,ppid=,rss=,cputime=").Output()
	if err != nil {
		return processMetricSnapshot{}, fmt.Errorf("read process metrics: %w", err)
	}
	processes := make(map[int]processMetric, len(strings.Split(string(output), "\n")))
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		pid, pidErr := strconv.Atoi(fields[0])
		parent, parentErr := strconv.Atoi(fields[1])
		rssKB, rssErr := strconv.ParseUint(fields[2], 10, 64)
		cpuSeconds, cpuErr := parseCPUTime(fields[3])
		if pidErr != nil || parentErr != nil || rssErr != nil || cpuErr != nil || pid <= 0 {
			continue
		}
		processes[pid] = processMetric{pid: pid, parent: parent, cpuSeconds: cpuSeconds, memoryBytes: rssKB * 1024}
	}
	if len(processes) == 0 {
		return processMetricSnapshot{}, errors.New("read process metrics: ps returned no parseable processes")
	}
	return processMetricSnapshot{processes: processes}, nil
}

func parseCPUTime(value string) (float64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, errors.New("empty CPU time")
	}
	var days float64
	if dayParts := strings.SplitN(value, "-", 2); len(dayParts) == 2 {
		parsed, err := strconv.ParseFloat(dayParts[0], 64)
		if err != nil {
			return 0, err
		}
		days = parsed
		value = dayParts[1]
	}
	parts := strings.Split(value, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, fmt.Errorf("invalid CPU time %q", value)
	}
	seconds, err := strconv.ParseFloat(parts[len(parts)-1], 64)
	if err != nil {
		return 0, err
	}
	minutes, err := strconv.ParseFloat(parts[len(parts)-2], 64)
	if err != nil {
		return 0, err
	}
	hours := 0.0
	if len(parts) == 3 {
		hours, err = strconv.ParseFloat(parts[0], 64)
		if err != nil {
			return 0, err
		}
	}
	return days*24*60*60 + hours*60*60 + minutes*60 + seconds, nil
}

func processTreeMetrics(processes map[int]processMetric, rootPID int) (cpuSeconds float64, memoryBytes uint64, found bool) {
	if _, ok := processes[rootPID]; !ok {
		return 0, 0, false
	}
	children := make(map[int][]int)
	for pid, process := range processes {
		children[process.parent] = append(children[process.parent], pid)
	}
	visited := make(map[int]bool)
	var visit func(int)
	visit = func(pid int) {
		if visited[pid] {
			return
		}
		process, ok := processes[pid]
		if !ok {
			return
		}
		visited[pid] = true
		cpuSeconds += process.cpuSeconds
		memoryBytes += process.memoryBytes
		for _, child := range children[pid] {
			visit(child)
		}
	}
	visit(rootPID)
	return cpuSeconds, memoryBytes, true
}

func hostMemoryCapacityBytes() (uint64, error) {
	output, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
	if err != nil {
		return 0, fmt.Errorf("read host memory size: %w", err)
	}
	memory, err := strconv.ParseUint(strings.TrimSpace(string(output)), 10, 64)
	if err != nil || memory == 0 {
		return 0, fmt.Errorf("parse host memory size %q", strings.TrimSpace(string(output)))
	}
	return memory, nil
}

func readNodeWorkingSet() (uint64, error) {
	total, err := hostMemoryCapacityBytes()
	if err != nil {
		return 0, err
	}
	output, err := exec.Command("vm_stat").Output()
	if err != nil {
		return 0, fmt.Errorf("read host VM statistics: %w", err)
	}
	pageSize, err := vmStatPageSize(string(output))
	if err != nil {
		return 0, err
	}
	freePages := vmStatPages(string(output), "Pages free")
	inactivePages := vmStatPages(string(output), "Pages inactive")
	speculativePages := vmStatPages(string(output), "Pages speculative")
	reclaimable := (freePages + inactivePages + speculativePages) * pageSize
	if reclaimable >= total {
		return 0, nil
	}
	return total - reclaimable, nil
}

func vmStatPageSize(output string) (uint64, error) {
	line := strings.Split(output, "\n")[0]
	start := strings.Index(line, "page size of ")
	if start < 0 {
		return 0, fmt.Errorf("parse vm_stat page size from %q", line)
	}
	value := strings.TrimSpace(line[start+len("page size of "):])
	value = strings.TrimSuffix(value, " bytes)")
	value = strings.TrimSuffix(value, " bytes")
	pageSize, err := strconv.ParseUint(value, 10, 64)
	if err != nil || pageSize == 0 {
		return 0, fmt.Errorf("parse vm_stat page size %q", value)
	}
	return pageSize, nil
}

func vmStatPages(output, name string) uint64 {
	prefix := name + ":"
	for _, line := range strings.Split(output, "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		value := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(strings.TrimPrefix(line, prefix)), "."))
		pages, err := strconv.ParseUint(value, 10, 64)
		if err == nil {
			return pages
		}
	}
	return 0
}

func formatResourceMetrics(now time.Time, nodeCPUSeconds float64, nodeMemoryBytes uint64, workloads []workloadMetric, scrapeErrors int) string {
	timestamp := now.UnixMilli()
	var output strings.Builder
	writeHelp := func(name, help, metricType string) {
		fmt.Fprintf(&output, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, metricType)
	}
	writeHelp("node_cpu_usage_seconds_total", "Cumulative cpu time consumed by the node in core-seconds", "counter")
	fmt.Fprintf(&output, "node_cpu_usage_seconds_total %s %d\n", formatMetricFloat(nodeCPUSeconds), timestamp)
	writeHelp("node_memory_working_set_bytes", "Current working set of the node in bytes", "gauge")
	fmt.Fprintf(&output, "node_memory_working_set_bytes %d %d\n", nodeMemoryBytes, timestamp)
	writeHelp("container_start_time_seconds", "Start time of the container since unix epoch in seconds", "gauge")
	writeHelp("container_cpu_usage_seconds_total", "Cumulative cpu time consumed by the container in core-seconds", "counter")
	writeHelp("container_memory_working_set_bytes", "Current working set of the container in bytes", "gauge")
	writeHelp("pod_cpu_usage_seconds_total", "Cumulative cpu time consumed by the pod in core-seconds", "counter")
	writeHelp("pod_memory_working_set_bytes", "Current working set of the pod in bytes", "gauge")
	for _, workload := range workloads {
		containerLabels := prometheusLabels(
			"container", workload.workload.PodContainerName,
			"pod", workload.workload.Name,
			"namespace", workload.workload.Namespace,
		)
		podLabels := prometheusLabels("pod", workload.workload.Name, "namespace", workload.workload.Namespace)
		fmt.Fprintf(&output, "container_start_time_seconds%s %s %d\n", containerLabels, formatMetricFloat(float64(workload.startTime.UnixNano())/float64(time.Second)), timestamp)
		fmt.Fprintf(&output, "container_cpu_usage_seconds_total%s %s %d\n", containerLabels, formatMetricFloat(workload.cpuSeconds), timestamp)
		fmt.Fprintf(&output, "container_memory_working_set_bytes%s %d %d\n", containerLabels, workload.memoryBytes, timestamp)
		fmt.Fprintf(&output, "pod_cpu_usage_seconds_total%s %s %d\n", podLabels, formatMetricFloat(workload.cpuSeconds), timestamp)
		fmt.Fprintf(&output, "pod_memory_working_set_bytes%s %d %d\n", podLabels, workload.memoryBytes, timestamp)
	}
	writeHelp("scrape_error", "1 if there was an error while getting container metrics, 0 otherwise", "gauge")
	writeHelp("resource_scrape_error", "1 if there was an error while getting container metrics, 0 otherwise", "gauge")
	fmt.Fprintf(&output, "scrape_error %d\nresource_scrape_error %d\n# EOF\n", boolInt(scrapeErrors != 0), boolInt(scrapeErrors != 0))
	return output.String()
}

func prometheusLabels(values ...string) string {
	var output strings.Builder
	output.WriteByte('{')
	for index := 0; index+1 < len(values); index += 2 {
		if index != 0 {
			output.WriteByte(',')
		}
		output.WriteString(values[index])
		output.WriteString("=\"")
		output.WriteString(escapePrometheusLabel(values[index+1]))
		output.WriteByte('"')
	}
	output.WriteByte('}')
	return output.String()
}

func escapePrometheusLabel(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	return strings.ReplaceAll(value, "\n", `\n`)
}

func formatMetricFloat(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func (h *kubeletHandler) serveResourceMetrics(response http.ResponseWriter, request *http.Request) {
	if h.metrics == nil {
		http.Error(response, "resource metrics are unavailable", http.StatusServiceUnavailable)
		return
	}
	body, err := h.metrics.collect(h.manager)
	if err != nil {
		http.Error(response, fmt.Sprintf("collect resource metrics: %v", err), http.StatusInternalServerError)
		return
	}
	response.Header().Set("Content-Type", "application/openmetrics-text; version=1.0.0; charset=utf-8")
	response.WriteHeader(http.StatusOK)
	_, _ = response.Write([]byte(body))
}
