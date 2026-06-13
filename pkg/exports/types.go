package exports

type ExportType string

const (
	TypeBinary     ExportType = "binary"
	TypeDockerRun  ExportType = "docker-run"
	TypeDockerExec ExportType = "docker-exec"
)

type ExportDef struct {
	Bin       string     `yaml:"bin" mapstructure:"bin"`
	Type      ExportType `yaml:"type" mapstructure:"type"`
	Target    string     `yaml:"target,omitempty" mapstructure:"target"`       // absolute path for native binary
	Container string     `yaml:"container,omitempty" mapstructure:"container"` // container name for docker-exec
	Command   string     `yaml:"command,omitempty" mapstructure:"command"`     // command inside container
	Image     string     `yaml:"image,omitempty" mapstructure:"image"`         // image tag for docker-run
	Workdir   string     `yaml:"workdir,omitempty" mapstructure:"workdir"`     // mount target for docker-run
}

type ProjectExports struct {
	Name    string      `yaml:"name" mapstructure:"name"`
	Path    string      `yaml:"path,omitempty" mapstructure:"path"`
	Exports []ExportDef `yaml:"exports" mapstructure:"exports"`
}

type Registry struct {
	Projects []ProjectExports `yaml:"projects" mapstructure:"projects"`
}
