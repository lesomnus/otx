# otx

Inject OpenTelemetry context.

## Usage

### Inject 
```go
import (
	"github.com/lesomnus/otx"
	"github.com/lesomnus/otx/otxgrpc"
	"google.golang.org/grpc"
)

func main() {
	tracer_provider := newTracerProvider()

	ctx := context.Background()
	my_otx := otx.New(otx.WithTracerProvider(tracer_provider))
	
	server := grpc.NewServer(
		grpc.StatsHandler(otxgrpc.NewServerHandler(my_otx))
	)
	
	// ...
}
```

### Extract
```go
import (
	"context"

	"github.com/lesomnus/otx"
	"github.com/lesomnus/otx/log"
)

// Your gRPC server implementation.
func (s *Server) GetFeature(ctx context.Context, point *Point) (*Feature, error) {
	ctx, span := otx.TraceStart(ctx, "getFeature")
	// or otx.Tracer(ctx).Start(context.TODO(), "getFeature")

	// slog.Logger is instantiated from the OpenTelemetry logger you injected
	// using "go.opentelemetry.io/contrib/bridges/otelslog"
	l := log.From(ctx)
	l.Info("hello")
	
	// ...
}

```
