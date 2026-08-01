package otxsdk

// The rest of this module is tested from otxsdk_test, through New. This file
// exists for the three functions that read the environment: they are
// unexported, and New collapses everything they distinguish into a single
// "New failed" observation - which list it parsed, which variable a protocol
// came from and which name it rejected are all invisible from the outside.

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// unsetenv removes env for the duration of the test.
//
// [testing.T.Setenv] cannot unset, and "unset" is the state every default in
// this file is specified for, distinct from "set to the empty string" only in
// that a future reader might tell them apart. Setting it first registers the
// restore that Setenv would have registered, and takes the same guard against
// being called from a parallel test.
func unsetenv(t *testing.T, env string) {
	t.Helper()

	t.Setenv(env, "")
	require.NoError(t, os.Unsetenv(env))
}

func TestExporterNames(t *testing.T) {
	const env = "OTEL_TRACES_EXPORTER"

	t.Run("unset selects a single otlp exporter", func(t *testing.T) {
		unsetenv(t, env)

		names, err := exporterNames(env)
		require.NoError(t, err)
		require.Equal(t, []string{"otlp"}, names)
	})
	t.Run("otlp selects the otlp exporter", func(t *testing.T) {
		t.Setenv(env, "otlp")

		names, err := exporterNames(env)
		require.NoError(t, err)
		require.Equal(t, []string{"otlp"}, names)
	})
	t.Run("console selects the console exporter", func(t *testing.T) {
		t.Setenv(env, "console")

		names, err := exporterNames(env)
		require.NoError(t, err)
		require.Equal(t, []string{"console"}, names)
	})
	t.Run("none selects nothing at all", func(t *testing.T) {
		t.Setenv(env, "none")

		names, err := exporterNames(env)
		require.NoError(t, err)
		require.Nil(t, names)
		require.Empty(t, names)
	})
	t.Run("a comma separated list keeps the order it was written in", func(t *testing.T) {
		for _, tc := range []struct {
			value string
			want  []string
		}{
			{"otlp,console", []string{"otlp", "console"}},
			{"console,otlp", []string{"console", "otlp"}},
			{"console,console", []string{"console", "console"}},
			{"otlp,console,otlp", []string{"otlp", "console", "otlp"}},
		} {
			t.Setenv(env, tc.value)

			names, err := exporterNames(env)
			require.NoErrorf(t, err, "%q", tc.value)
			require.Equalf(t, tc.want, names, "%q", tc.value)
		}
	})
	t.Run("whitespace and case are tolerated", func(t *testing.T) {
		for _, tc := range []struct {
			value string
			want  []string
		}{
			{"  otlp  ", []string{"otlp"}},
			{"OTLP", []string{"otlp"}},
			{"Console", []string{"console"}},
			{"\tCONSOLE\n", []string{"console"}},
			{" otlp , CONSOLE ", []string{"otlp", "console"}},
			{"otlp,\n\tconsole", []string{"otlp", "console"}},
		} {
			t.Setenv(env, tc.value)

			names, err := exporterNames(env)
			require.NoErrorf(t, err, "%q", tc.value)
			require.Equalf(t, tc.want, names, "%q", tc.value)
		}
	})
	t.Run("none anywhere in a list wins over everything else", func(t *testing.T) {
		for _, value := range []string{
			"none",
			"none,otlp",
			"otlp,none",
			"console,none,otlp",
			"otlp,console,none",
			" NONE ",
			"otlp, None ",
			// The scan stops at none, so a name that would otherwise be
			// rejected is never even looked at once none has been seen.
			"none,jaeger",
		} {
			t.Setenv(env, value)

			names, err := exporterNames(env)
			require.NoErrorf(t, err, "%q", value)
			require.Nilf(t, names, "%q", value)
		}
	})
	t.Run("an empty value falls back to otlp", func(t *testing.T) {
		for _, value := range []string{
			"",
			"   ",
			"\t\n",
			",",
			",,,",
			" , , ",
		} {
			t.Setenv(env, value)

			names, err := exporterNames(env)
			require.NoErrorf(t, err, "%q", value)
			require.Equalf(t, []string{"otlp"}, names, "%q", value)
		}
	})
	t.Run("an unknown name is an error naming the variable and the value", func(t *testing.T) {
		t.Setenv(env, "jaeger")

		names, err := exporterNames(env)
		require.Error(t, err)
		require.Nil(t, names)
		require.ErrorContains(t, err, env)
		require.ErrorContains(t, err, `"jaeger"`)

		// And it says what would have been accepted.
		require.ErrorContains(t, err, `"otlp"`)
		require.ErrorContains(t, err, `"console"`)
		require.ErrorContains(t, err, `"none"`)
	})
	t.Run("the rejected value is reported trimmed and lowercased", func(t *testing.T) {
		// What the message quotes is the name after normalisation rather than
		// the raw text, so " Jaeger " is reported as "jaeger".
		t.Setenv(env, "  Jaeger  ")

		_, err := exporterNames(env)
		require.ErrorContains(t, err, `"jaeger"`)
	})
	t.Run("an unknown name anywhere in a list is an error", func(t *testing.T) {
		for _, value := range []string{
			"jaeger",
			"otlp,jaeger",
			"jaeger,otlp",
			"otlp,jaeger,console",
			// none is only a winner once it has been reached.
			"jaeger,none",
		} {
			t.Setenv(env, value)

			names, err := exporterNames(env)
			require.ErrorContainsf(t, err, "jaeger", "%q", value)
			require.Nilf(t, names, "%q", value)
		}
	})
	t.Run("the variable named in the error is the one that was read", func(t *testing.T) {
		// Each signal has its own variable and the message has to point at the
		// right one, so the name is taken from the argument rather than baked
		// into the message.
		for _, name := range []string{
			"OTEL_TRACES_EXPORTER",
			"OTEL_METRICS_EXPORTER",
			"OTEL_LOGS_EXPORTER",
		} {
			t.Setenv(name, "jaeger")

			_, err := exporterNames(name)
			require.ErrorContains(t, err, name)
		}
	})
	t.Run("each signal reads only its own variable", func(t *testing.T) {
		t.Setenv("OTEL_TRACES_EXPORTER", "console")
		t.Setenv("OTEL_METRICS_EXPORTER", "none")
		unsetenv(t, "OTEL_LOGS_EXPORTER")

		traces, err := exporterNames("OTEL_TRACES_EXPORTER")
		require.NoError(t, err)
		require.Equal(t, []string{"console"}, traces)

		metrics, err := exporterNames("OTEL_METRICS_EXPORTER")
		require.NoError(t, err)
		require.Nil(t, metrics)

		logs, err := exporterNames("OTEL_LOGS_EXPORTER")
		require.NoError(t, err)
		require.Equal(t, []string{"otlp"}, logs)
	})
}

func TestProtocolName(t *testing.T) {
	const (
		env     = "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL"
		generic = "OTEL_EXPORTER_OTLP_PROTOCOL"
	)

	t.Run("unset is http/protobuf", func(t *testing.T) {
		unsetenv(t, env)
		unsetenv(t, generic)

		protocol, err := protocolName(env)
		require.NoError(t, err)
		require.Equal(t, "http/protobuf", protocol)
	})
	t.Run("an empty value on both is http/protobuf", func(t *testing.T) {
		for _, tc := range [][2]string{
			{"", ""},
			{"   ", ""},
			{"", "  "},
			{" \t ", " \n "},
		} {
			t.Setenv(env, tc[0])
			t.Setenv(generic, tc[1])

			protocol, err := protocolName(env)
			require.NoErrorf(t, err, "%q %q", tc[0], tc[1])
			require.Equalf(t, "http/protobuf", protocol, "%q %q", tc[0], tc[1])
		}
	})
	t.Run("both protocols are accepted", func(t *testing.T) {
		for _, value := range []string{"grpc", "http/protobuf"} {
			t.Setenv(env, value)
			unsetenv(t, generic)

			protocol, err := protocolName(env)
			require.NoErrorf(t, err, "%q", value)
			require.Equalf(t, value, protocol, "%q", value)
		}
	})
	t.Run("surrounding whitespace is trimmed", func(t *testing.T) {
		t.Setenv(env, "  grpc\n")
		unsetenv(t, generic)

		protocol, err := protocolName(env)
		require.NoError(t, err)
		require.Equal(t, "grpc", protocol)
	})
	t.Run("the per signal variable wins over the generic one", func(t *testing.T) {
		t.Setenv(generic, "http/protobuf")
		t.Setenv(env, "grpc")

		protocol, err := protocolName(env)
		require.NoError(t, err)
		require.Equal(t, "grpc", protocol)

		// And the other way round, so the test cannot pass because grpc simply
		// wins wherever it is written.
		t.Setenv(generic, "grpc")
		t.Setenv(env, "http/protobuf")

		protocol, err = protocolName(env)
		require.NoError(t, err)
		require.Equal(t, "http/protobuf", protocol)
	})
	t.Run("the generic variable is used when the per signal one is unset", func(t *testing.T) {
		unsetenv(t, env)
		t.Setenv(generic, "grpc")

		protocol, err := protocolName(env)
		require.NoError(t, err)
		require.Equal(t, "grpc", protocol)
	})
	t.Run("a blank per signal variable falls through to the generic one", func(t *testing.T) {
		// Blank is treated as unset rather than as an override to nothing,
		// which is what makes OTEL_EXPORTER_OTLP_TRACES_PROTOCOL= harmless.
		t.Setenv(env, "   ")
		t.Setenv(generic, "grpc")

		protocol, err := protocolName(env)
		require.NoError(t, err)
		require.Equal(t, "grpc", protocol)
	})
	t.Run("each signal reads only its own variable", func(t *testing.T) {
		unsetenv(t, generic)
		t.Setenv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "grpc")
		unsetenv(t, "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL")
		t.Setenv("OTEL_EXPORTER_OTLP_LOGS_PROTOCOL", "http/protobuf")

		traces, err := protocolName("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL")
		require.NoError(t, err)
		require.Equal(t, "grpc", traces)

		metrics, err := protocolName("OTEL_EXPORTER_OTLP_METRICS_PROTOCOL")
		require.NoError(t, err)
		require.Equal(t, "http/protobuf", metrics)

		logs, err := protocolName("OTEL_EXPORTER_OTLP_LOGS_PROTOCOL")
		require.NoError(t, err)
		require.Equal(t, "http/protobuf", logs)
	})
	t.Run("http/json is rejected", func(t *testing.T) {
		// It is a protocol the specification lists but the Go exporters do not
		// implement, so it has to fail loudly instead of being downgraded.
		t.Setenv(env, "http/json")
		unsetenv(t, generic)

		protocol, err := protocolName(env)
		require.Error(t, err)
		require.Empty(t, protocol)
		require.ErrorContains(t, err, `"http/json"`)
		require.ErrorContains(t, err, `"http/protobuf"`)
		require.ErrorContains(t, err, `"grpc"`)
	})
	t.Run("the error names the per signal variable when that is the one that was set", func(t *testing.T) {
		t.Setenv(env, "http/json")
		unsetenv(t, generic)

		_, err := protocolName(env)
		require.ErrorContains(t, err, env)
		require.NotContains(t, err.Error(), generic+":")
	})
	t.Run("the error names the generic variable when that is the one that was set", func(t *testing.T) {
		unsetenv(t, env)
		t.Setenv(generic, "http/json")

		_, err := protocolName(env)
		require.ErrorContains(t, err, generic)
		require.NotContains(t, err.Error(), env)
	})
	t.Run("the error names the per signal variable when both are set", func(t *testing.T) {
		// The value that was read is the per signal one, so that is what the
		// message has to point the reader at, even though the generic one is
		// just as wrong.
		t.Setenv(env, "http/json")
		t.Setenv(generic, "http/json")

		_, err := protocolName(env)
		require.ErrorContains(t, err, env)
	})
	t.Run("a bad generic value is not reached when the per signal one is valid", func(t *testing.T) {
		t.Setenv(env, "grpc")
		t.Setenv(generic, "http/json")

		protocol, err := protocolName(env)
		require.NoError(t, err)
		require.Equal(t, "grpc", protocol)
	})
	t.Run("the protocol is matched case insensitively", func(t *testing.T) {
		// As forgiving as an exporter name, which is lowercased before it is
		// matched. The specification writes both in lower case, so neither
		// spelling is wrong to accept.
		for _, tc := range []struct {
			value string
			want  string
		}{
			{value: "GRPC", want: protocolGRPC},
			{value: "Grpc", want: protocolGRPC},
			{value: " grpc ", want: protocolGRPC},
			{value: "HTTP/PROTOBUF", want: protocolHTTP},
			{value: "Http/Protobuf", want: protocolHTTP},
		} {
			t.Setenv(env, tc.value)
			unsetenv(t, generic)

			protocol, err := protocolName(env)
			require.NoErrorf(t, err, "%q", tc.value)
			require.Equalf(t, tc.want, protocol, "%q", tc.value)
		}
	})
	t.Run("any other value is rejected", func(t *testing.T) {
		for _, value := range []string{"http", "protobuf", "thrift", "http/protobuf,grpc"} {
			t.Setenv(env, value)
			unsetenv(t, generic)

			protocol, err := protocolName(env)
			require.Errorf(t, err, "%q", value)
			require.Emptyf(t, protocol, "%q", value)
			require.ErrorContainsf(t, err, env, "%q", value)
		}
	})
}

func TestDisabled(t *testing.T) {
	const env = "OTEL_SDK_DISABLED"

	t.Run("unset is not disabled", func(t *testing.T) {
		unsetenv(t, env)
		require.False(t, disabled())
	})
	t.Run("true in any case is disabled", func(t *testing.T) {
		for _, value := range []string{
			"true",
			"TRUE",
			"True",
			"tRuE",
			" true ",
			"\ttrue\n",
			"   TRUE   ",
		} {
			t.Setenv(env, value)
			require.Truef(t, disabled(), "%q", value)
		}
	})
	t.Run("anything else is not disabled", func(t *testing.T) {
		// Only the one word the specification defines turns the SDK off; a
		// value that merely looks truthy leaves telemetry on, because guessing
		// wrong here silently drops every signal.
		for _, value := range []string{
			"",
			"   ",
			"false",
			"FALSE",
			"1",
			"0",
			"yes",
			"on",
			"t",
			"true1",
			"not true",
			"true false",
		} {
			t.Setenv(env, value)
			require.Falsef(t, disabled(), "%q", value)
		}
	})
}
