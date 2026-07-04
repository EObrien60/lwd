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
// resolvedSecrets maps a secret name (as referenced in a Service.Secrets
// entry) to its resolved plaintext value; the caller is expected to have
// already resolved secrets fail-closed (see SecretResolver) before calling.
//
// RenderBackingCompose returns ("", "") if there are no services to render.
func RenderBackingCompose(appName string, services []spec.Service, resolvedSecrets map[string]string) (yaml string, network string) {
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

		env := mergedEnv(svc, resolvedSecrets)
		if len(env) > 0 {
			b.WriteString("    environment:\n")
			keys := make([]string, 0, len(env))
			for k := range env {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				fmt.Fprintf(&b, "      %s: %s\n", yamlQuote(k), yamlQuote(env[k]))
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

// mergedEnv merges a service's declared literal env with its declared
// secrets resolved against resolvedSecrets. A secret name absent from
// resolvedSecrets is skipped (the caller resolves secrets fail-closed
// before calling; RenderBackingCompose stays pure and does not itself
// error).
func mergedEnv(svc spec.Service, resolvedSecrets map[string]string) map[string]string {
	if len(svc.Env) == 0 && len(svc.Secrets) == 0 {
		return nil
	}
	env := make(map[string]string, len(svc.Env)+len(svc.Secrets))
	for k, v := range svc.Env {
		env[k] = v
	}
	for _, name := range svc.Secrets {
		if v, ok := resolvedSecrets[name]; ok {
			env[name] = v
		}
	}
	return env
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
