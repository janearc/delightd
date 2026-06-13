package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// GenerateCLIWrapper creates a bash script at varBinDir/delight that acts as a router
// to the aggregated skills based on the AWS CLI pattern `delight <service> <tool>`.
func GenerateCLIWrapper(varBinDir string, tools []Tool) error {
	if err := os.MkdirAll(varBinDir, 0755); err != nil {
		return err
	}

	cliPath := filepath.Join(varBinDir, "delight")

	var sb strings.Builder
	sb.WriteString("#!/usr/bin/env bash\n\n")
	sb.WriteString("SERVICE=$1\n")
	sb.WriteString("ACTION=$2\n")
	sb.WriteString("shift 2\n\n")

	sb.WriteString("case \"${SERVICE}_${ACTION}\" in\n")

	for _, t := range tools {
		sb.WriteString(fmt.Sprintf("  \"%s\")\n", t.Name))

		switch t.Handler.Type {
		case "command":
			argsStr := strings.Join(t.Handler.Args, " ")
			sb.WriteString(fmt.Sprintf("    exec %s %s \"$@\"\n", t.Handler.Command, argsStr))
		case "http":
			sb.WriteString(fmt.Sprintf("    curl -s -X %s \"%s\" -d \"$*\"\n", t.Handler.Method, t.Handler.URL))
		case "internal":
			if t.Handler.Method == "backup" {
				sb.WriteString("    curl -s -X POST \"http://localhost:8088/projects/$1/backup\"\n")
			}
		default:
			sb.WriteString("    echo 'unsupported handler type'\n")
		}

		sb.WriteString("    ;;\n")
	}

	sb.WriteString("  *)\n")
	sb.WriteString("    echo 'Usage: delight <service> <action> [args]'\n")
	sb.WriteString("    echo 'Available commands:'\n")
	for _, t := range tools {
		// Split service_action back to service action for display
		parts := strings.SplitN(t.Name, "_", 2)
		if len(parts) == 2 {
			sb.WriteString(fmt.Sprintf("    echo '  delight %s %s - %s'\n", parts[0], parts[1], t.Description))
		}
	}
	sb.WriteString("    exit 1\n")
	sb.WriteString("    ;;\n")
	sb.WriteString("esac\n")

	return os.WriteFile(cliPath, []byte(sb.String()), 0755)
}
