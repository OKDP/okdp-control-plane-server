package models

type SparkAppRequest struct {
	Name                string            `json:"name" binding:"required"`
	Type                string            `json:"type" binding:"required"`
	Mode                string            `json:"mode"`
	Image               string            `json:"image" binding:"required"`
	MainClass           string            `json:"mainClass,omitempty"`
	MainApplicationFile string            `json:"mainApplicationFile,omitempty"`
	Arguments           []string          `json:"arguments,omitempty"`
	SparkVersion        string            `json:"sparkVersion,omitempty"`
	DriverCores         *int32            `json:"driverCores,omitempty"`
	DriverMemory        string            `json:"driverMemory,omitempty"`
	ExecutorInstances   *int32            `json:"executorInstances,omitempty"`
	ExecutorCores       *int32            `json:"executorCores,omitempty"`
	ExecutorMemory      string            `json:"executorMemory,omitempty"`
	SparkConf           map[string]string `json:"sparkConf,omitempty"`
}

type SparkAppYAMLRequest struct {
	YAML string `json:"yaml" binding:"required"`
}

type SparkAppInstance struct {
	Name          string            `json:"name"`
	Type          string            `json:"type"`
	Mode          string            `json:"mode"`
	Image         string            `json:"image"`
	Status        string            `json:"status"`
	ErrorMessage  string            `json:"errorMessage,omitempty"`
	DriverPodName string            `json:"driverPodName,omitempty"`
	CreatedAt     string            `json:"createdAt,omitempty"`
	CompletedAt   string            `json:"completedAt,omitempty"`
	Executors     map[string]string `json:"executors,omitempty"`
}

type SparkAppUpdateRequest struct {
	Image               string            `json:"image,omitempty"`
	MainClass           string            `json:"mainClass,omitempty"`
	MainApplicationFile string            `json:"mainApplicationFile,omitempty"`
	Arguments           []string          `json:"arguments,omitempty"`
	DriverCores         *int32            `json:"driverCores,omitempty"`
	DriverMemory        string            `json:"driverMemory,omitempty"`
	ExecutorInstances   *int32            `json:"executorInstances,omitempty"`
	ExecutorCores       *int32            `json:"executorCores,omitempty"`
	ExecutorMemory      string            `json:"executorMemory,omitempty"`
	SparkConf           map[string]string `json:"sparkConf,omitempty"`
}

type SparkUIInfo struct {
	ServiceName      string `json:"serviceName"`
	UIAddress        string `json:"uiAddress"`
	HistoryServerURL string `json:"historyServerUrl"`
	Available        bool   `json:"available"`
}

type SparkConfig struct {
	Image SparkConfigImage `json:"image"`
	Spark SparkConfigSpark `json:"spark"`
}

type SparkConfigImage struct {
	Registry   string `json:"registry"`
	Repository string `json:"repository"`
	Tag        string `json:"tag"`
}

type SparkConfigSpark struct {
	DefaultVersion string        `json:"defaultVersion"`
	Images         []SparkImage  `json:"images"`
	Defaults       SparkDefaults `json:"defaults"`
}

type SparkImage struct {
	Label string `json:"label"`
	Image string `json:"image"`
}

type SparkDefaults struct {
	Driver   ResourceDefaults `json:"driver"`
	Executor ExecutorDefaults `json:"executor"`
}

type ResourceDefaults struct {
	Cores  int    `json:"cores"`
	Memory string `json:"memory"`
}

type ExecutorDefaults struct {
	ResourceDefaults `json:",inline"`
	Instances        int `json:"instances"`
}
