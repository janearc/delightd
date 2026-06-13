package exports

import (
	"strings"
	"testing"
)

func TestGenerateWrapper(t *testing.T) {
	tests := []struct {
		name    string
		def     ExportDef
		want    string
		wantErr bool
	}{
		{
			name: "docker-run wrapper",
			def: ExportDef{
				Type:    TypeDockerRun,
				Image:   "test-image:latest",
				Workdir: "/workspace",
			},
			want:    "exec docker run --rm -i \\\n  -v \"$(pwd):/workspace\" \\\n  -w \"/workspace\" \\\n  \"test-image:latest\" \"$@\"\n",
			wantErr: false,
		},
		{
			name: "docker-exec wrapper",
			def: ExportDef{
				Type:      TypeDockerExec,
				Container: "test-container",
				Command:   "/app/cli",
			},
			want:    "exec docker exec -i \"test-container\" /app/cli \"$@\"\n",
			wantErr: false,
		},
		{
			name: "invalid type",
			def: ExportDef{
				Type: TypeBinary, // Should error out as Binary doesn't need a wrapper
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := GenerateWrapper(tt.def)
			if (err != nil) != tt.wantErr {
				t.Errorf("GenerateWrapper() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && !strings.Contains(got, tt.want) {
				t.Errorf("GenerateWrapper() got = %v, want to contain %v", got, tt.want)
			}
		})
	}
}
