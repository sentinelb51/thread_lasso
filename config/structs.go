package config

type Config struct {
	Games map[string]Game `json:"games"`
}

type Game struct {
	Priority    string   `json:"priority,omitempty"`     // "realtime", "high", "above_normal", "normal", "below_normal", "idle"
	IOPriority  string   `json:"io_priority,omitempty"`  // "high", "normal", "low", "very_low"
	GPUPriority string   `json:"gpu_priority,omitempty"` // "realtime", "high", "normal", "below_normal", "low"
	Affinity    []int    `json:"affinity,omitempty"`
	CPUSets     []int    `json:"cpu_sets,omitempty"`
	Threads     []Thread `json:"threads,omitempty"`
}

type Thread struct {
	Name       string `json:"name"`
	Priority   *int   `json:"priority,omitempty"`    // -15, -2, -1, 0, 1, 2, 15
	IOPriority *int   `json:"io_priority,omitempty"` // 0 - 3 = Very Low, Low, Normal, High
	Affinity   []int  `json:"affinity,omitempty"`
	CPUSets    []int  `json:"cpu_sets,omitempty"`
}
