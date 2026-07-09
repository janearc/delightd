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
	// The control port bearer-gates every mutation; a generated mutating call reads the token
	// from the creds mount and passes it via a one-shot curl config so it never lands on
	// curl's argv (ps/logs). printf is a builtin, so the token is not a process argument
	// anywhere. A missing token fails loud rather than sending an unauthenticated mutation.
	sb.WriteString("CONTROL_TOKEN_FILE=\"${DELIGHT_CONTROL_TOKEN_FILE:-/run/delightd/control-token}\"\n")
	sb.WriteString("_authcfg() {\n")
	sb.WriteString("  [ -s \"$CONTROL_TOKEN_FILE\" ] || { echo \"control token not provisioned at $CONTROL_TOKEN_FILE\" >&2; exit 1; }\n")
	sb.WriteString("  local cfg; cfg=$(mktemp) || { echo 'cannot create temp curl config' >&2; exit 1; }\n")
	sb.WriteString("  chmod 600 \"$cfg\"\n")
	sb.WriteString("  printf 'header = \"Authorization: Bearer %s\"\\n' \"$(cat \"$CONTROL_TOKEN_FILE\")\" >\"$cfg\"\n")
	sb.WriteString("  printf '%s' \"$cfg\"\n")
	sb.WriteString("}\n\n")
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
			// Map each {name} path param onto a positional arg ($1, $2, ...), so
			// `delight delightd furnish_up <piece>` lands the piece in the route path.
			// Non-parameterized URLs render unchanged. A mutating verb carries the
			// control-port bearer (via a one-shot curl config); a read carries none.
			if isMutatingMethod(t.Handler.Method) {
				fmt.Fprintf(&sb, "    cfg=$(_authcfg); curl -s -K \"$cfg\" -X %s \"%s\" -d \"$*\"; rm -f \"$cfg\"\n", t.Handler.Method, positionalURL(t.Handler.URL))
			} else {
				fmt.Fprintf(&sb, "    curl -s -X %s \"%s\" -d \"$*\"\n", t.Handler.Method, positionalURL(t.Handler.URL))
			}
		case "internal":
			if t.Handler.Method == "backup" {
				// backup is a mutation: carry the bearer.
				sb.WriteString("    cfg=$(_authcfg); curl -s -K \"$cfg\" -X POST \"http://localhost:8088/projects/$1/backup\"; rm -f \"$cfg\"\n")
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
