package otxsdk

import (
	"fmt"
	"os"
	"strings"
)

// Exporter names of the OTEL_{TRACES,METRICS,LOGS}_EXPORTER variables.
const (
	exporterOTLP    = "otlp"
	exporterConsole = "console"
	exporterNone    = "none"
)

// Protocol names of the OTEL_EXPORTER_OTLP_PROTOCOL family.
const (
	protocolGRPC = "grpc"
	protocolHTTP = "http/protobuf"
)

// disabled reports whether OTEL_SDK_DISABLED asks for no telemetry at all.
func disabled() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("OTEL_SDK_DISABLED")), "true")
}

// exporterNames returns the exporters selected for one signal, in the order
// they were listed. The default is a single OTLP exporter, as the
// specification requires. "none" anywhere in the list means no exporter at
// all, whatever else is listed.
func exporterNames(env string) ([]string, error) {
	v := strings.TrimSpace(os.Getenv(env))
	if v == "" {
		return []string{exporterOTLP}, nil
	}

	names := []string{}
	for _, name := range strings.Split(v, ",") {
		name = strings.ToLower(strings.TrimSpace(name))
		switch name {
		case "":
			continue

		case exporterNone:
			return nil, nil

		case exporterOTLP, exporterConsole:
			names = append(names, name)

		default:
			return nil, fmt.Errorf("%s: unsupported exporter %q, want one of %q, %q or %q",
				env, name, exporterOTLP, exporterConsole, exporterNone)
		}
	}
	if len(names) == 0 {
		return []string{exporterOTLP}, nil
	}

	return names, nil
}

// protocolName returns the OTLP protocol for one signal: the signal's own
// variable if set, then OTEL_EXPORTER_OTLP_PROTOCOL, then the specified
// default of http/protobuf.
func protocolName(env string) (string, error) {
	// Lower cased like the exporter names, so that the two variables are
	// equally forgiving; the specification writes both in lower case.
	v := strings.ToLower(strings.TrimSpace(os.Getenv(env)))
	from := env
	if v == "" {
		v = strings.ToLower(strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL")))
		from = "OTEL_EXPORTER_OTLP_PROTOCOL"
	}
	if v == "" {
		return protocolHTTP, nil
	}

	switch v {
	case protocolGRPC, protocolHTTP:
		return v, nil

	default:
		// http/json is a specified protocol that the Go exporters do not
		// implement, so it is reported as unsupported rather than silently
		// downgraded.
		return "", fmt.Errorf("%s: unsupported protocol %q, want %q or %q",
			from, v, protocolHTTP, protocolGRPC)
	}
}
