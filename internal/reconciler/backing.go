package reconciler

import (
	"fmt"
	"sort"
	"strings"

	"lwd/internal/spec"
)

// RenderBackingCompose renders an app's declared backing services into a
// compose v3-ish YAML document, pure and deterministic: given the same
// inputs it always produces byte-identical output. Backing services run
// PINNED (never blue-greened) on a dedicated per-app network and publish no
// host ports — only surfaces on the shared lwd network are reachable from
// outside; backing services are internal-only, reached by container name on
// the per-app network.
//
// RenderBackingCompose never bakes a resolved secret VALUE into the output —
// only a name/reference. Each name in a service's declared Secrets is
// rendered as a `${NAME}` compose-interpolation reference in that service's
// environment block; `docker compose up` resolves it at up-time from the
// process env the caller passes as UpSpec.Env (see (*Reconciler).ensureBacking).
// This mirrors how Phase 4 stores a user's compose file with ${VAR} refs, not
// resolved values: the rendered YAML here is what gets persisted verbatim to
// store.Deployment.Compose (plaintext) and served back over the API, so it
// must never contain a secret's plaintext value.
//
// RenderBackingCompose returns ("", "") if there are no services to render.
func RenderBackingCompose(appName string, services []spec.Service) (yaml string, network string) {
	if len(services) == 0 {
		return "", ""
	}

	network = "lwd-" + appName

	sorted := append([]spec.Service(nil), services...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	// Collect named top-level volumes (Volume of the form "name:path" where
	// name has no "/" — i.e. not a bind mount or anonymous path).
	var namedVolumes []string
	seenVolumes := make(map[string]bool)
	for _, svc := range sorted {
		if name := namedVolumeOf(svc.Volume); name != "" && !seenVolumes[name] {
			seenVolumes[name] = true
			namedVolumes = append(namedVolumes, name)
		}
	}
	sort.Strings(namedVolumes)

	var b strings.Builder

	b.WriteString("networks:\n")
	fmt.Fprintf(&b, "  %s:\n", yamlQuote(network))

	if len(namedVolumes) > 0 {
		b.WriteString("volumes:\n")
		for _, v := range namedVolumes {
			fmt.Fprintf(&b, "  %s:\n", yamlQuote(v))
		}
	}

	b.WriteString("services:\n")
	for _, svc := range sorted {
		fmt.Fprintf(&b, "  %s:\n", yamlQuote(svc.Name))
		fmt.Fprintf(&b, "    image: %s\n", yamlQuote(svc.Image))
		if svc.Command != "" {
			fmt.Fprintf(&b, "    command: %s\n", yamlQuote(svc.Command))
		}

		if len(svc.Env) > 0 || len(svc.Secrets) > 0 {
			secretSet := make(map[string]bool, len(svc.Secrets))
			for _, name := range svc.Secrets {
				secretSet[name] = true
			}
			keySet := make(map[string]bool, len(svc.Env)+len(svc.Secrets))
			for k := range svc.Env {
				keySet[k] = true
			}
			for name := range secretSet {
				keySet[name] = true
			}
			keys := make([]string, 0, len(keySet))
			for k := range keySet {
				keys = append(keys, k)
			}
			sort.Strings(keys)

			b.WriteString("    environment:\n")
			for _, k := range keys {
				if secretSet[k] {
					// A compose-interpolation REFERENCE, not the resolved
					// value: docker compose substitutes ${k} from its own
					// process env (UpSpec.Env) at `up` time. Deliberately
					// NOT passed through yamlQuote's value-escaping (no
					// $$-doubling) so compose actually interpolates it; only
					// the key goes through yamlQuote for injection-safety.
					fmt.Fprintf(&b, "      %s: \"${%s}\"\n", yamlQuote(k), k)
				} else {
					// A literal from Service.Env: secrets override env on
					// key collision, so this branch is only reached for keys
					// not also declared as a secret.
					fmt.Fprintf(&b, "      %s: %s\n", yamlQuote(k), yamlQuote(svc.Env[k]))
				}
			}
		}

		if svc.Volume != "" {
			fmt.Fprintf(&b, "    volumes:\n      - %s\n", yamlQuote(svc.Volume))
		}

		fmt.Fprintf(&b, "    networks:\n      - %s\n", yamlQuote(network))
		b.WriteString("    restart: unless-stopped\n")
	}

	return b.String(), network
}

// namedVolumeOf returns the top-level named-volume name declared by a
// Service.Volume spec, or "" if the volume is a bind mount, anonymous path,
// or unset. Volume is expected in "name:path" form for a named volume; a
// bind mount looks like "/host/path:/container/path" (first segment has a
// "/", so it's not a named volume).
func namedVolumeOf(volume string) string {
	if volume == "" {
		return ""
	}
	parts := strings.SplitN(volume, ":", 2)
	if len(parts) != 2 {
		return ""
	}
	name := parts[0]
	if name == "" || strings.Contains(name, "/") {
		return ""
	}
	return name
}

// yamlQuote renders s as a double-quoted YAML scalar that round-trips ANY
// content safely, regardless of embedded structure or control characters
// (secrets, env keys/values, and other dynamic strings may contain
// arbitrary bytes — this must never allow escaping the quoted scalar or
// injecting YAML structure).
//
// Order matters: backslash must be escaped first, before any of the other
// backslash-based escapes are introduced, so those escapes aren't
// themselves re-escaped.
func yamlQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	s = strings.ReplaceAll(s, "\r", `\r`)
	s = strings.ReplaceAll(s, "\t", `\t`)
	// docker compose re-interpolates ${...}/$VAR over the rendered file
	// text; double every literal $ so our already-final values are not
	// re-interpolated (and can't leak/corrupt values containing a $).
	s = strings.ReplaceAll(s, `$`, `$$`)
	return `"` + s + `"`
}
