package dbtracer

import (
	"context"
	"log/slog"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/trace/noop"
)

// recordingHandler keeps every record it is handed so a test can assert on
// what was actually logged.
type recordingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}

func (h *recordingHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }

func (h *recordingHandler) WithGroup(_ string) slog.Handler { return h }

func (h *recordingHandler) all() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]slog.Record(nil), h.records...)
}

func (h *recordingHandler) messages() []string {
	msgs := []string{}
	for _, r := range h.all() {
		msgs = append(msgs, r.Message)
	}
	return msgs
}

// newTracerWithoutTracing builds a tracer with a real meter provider and a
// recording logger, but no usable TracerProvider.
func newTracerWithoutTracing(t *testing.T, opts ...Option) (Tracer, *recordingHandler, *sdkmetric.ManualReader) {
	t.Helper()

	logs := &recordingHandler{}
	reader := sdkmetric.NewManualReader()

	base := []Option{
		WithMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))),
		WithLogger(slog.New(logs)),
		WithShouldLog(func(error) bool { return true }),
	}

	tracer, err := NewDBTracer("test_db", append(base, opts...)...)
	require.NoError(t, err)

	return tracer, logs, reader
}

// Logging and tracing are independent features. Someone who wants query logs
// should get them without standing up a TracerProvider first.
func TestTraceQueryEndLogsWhenTracingIsNotConfigured(t *testing.T) {
	const sql = `-- name: get_users :one
SELECT * FROM users WHERE id = $1`

	tests := []struct {
		name string
		opts []Option
	}{
		{
			// No WithTraceProvider: falls back to otel.GetTracerProvider(),
			// which is a no-op until the application installs a real one.
			name: "tracer provider left at the global default",
		},
		{
			name: "explicit no-op tracer provider",
			opts: []Option{WithTraceProvider(noop.NewTracerProvider())},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracer, logs, reader := newTracerWithoutTracing(t, tt.opts...)

			ctx := tracer.TraceQueryStart(context.Background(), &pgx.Conn{}, pgx.TraceQueryStartData{
				SQL:  sql,
				Args: []any{1},
			})
			tracer.TraceQueryEnd(ctx, &pgx.Conn{}, pgx.TraceQueryEndData{
				CommandTag: pgconn.CommandTag{},
				Err:        nil,
			})

			records := logs.all()
			require.Len(t, records, 1,
				"query should be logged without a tracer provider, logged: %v", logs.messages())
			assert.Equal(t, "query", records[0].Message)
			assert.Equal(t, slog.LevelInfo, records[0].Level)

			// The duration metric is recorded before the span is inspected, so
			// it still lands. This pins the failure to logging specifically:
			// the hook ran, it just gave up before reaching the log call.
			var rm metricdata.ResourceMetrics
			require.NoError(t, reader.Collect(context.Background(), &rm))
			assert.NotEmpty(t, rm.ScopeMetrics, "duration metric should still be recorded")
		})
	}
}

// Every other hook guards its body the same way, so none of them log either.
func TestAllHooksLogWhenTracingIsNotConfigured(t *testing.T) {
	const sql = `-- name: get_users :one
SELECT * FROM users WHERE id = $1`

	tests := []struct {
		name    string
		wantMsg string
		run     func(tracer Tracer)
	}{
		{
			name:    "query",
			wantMsg: "query",
			run: func(tracer Tracer) {
				ctx := tracer.TraceQueryStart(context.Background(), &pgx.Conn{},
					pgx.TraceQueryStartData{SQL: sql})
				tracer.TraceQueryEnd(ctx, &pgx.Conn{}, pgx.TraceQueryEndData{})
			},
		},
		{
			name:    "prepare",
			wantMsg: "prepare",
			run: func(tracer Tracer) {
				ctx := tracer.TracePrepareStart(context.Background(), &pgx.Conn{},
					pgx.TracePrepareStartData{Name: "get_users", SQL: sql})
				tracer.TracePrepareEnd(ctx, &pgx.Conn{}, pgx.TracePrepareEndData{})
			},
		},
		{
			name:    "acquire",
			wantMsg: "acquire connection",
			run: func(tracer Tracer) {
				ctx := tracer.TraceAcquireStart(context.Background(), &pgxpool.Pool{},
					pgxpool.TraceAcquireStartData{})
				tracer.TraceAcquireEnd(ctx, &pgxpool.Pool{}, pgxpool.TraceAcquireEndData{})
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracer, logs, _ := newTracerWithoutTracing(t, WithTraceProvider(noop.NewTracerProvider()))

			tt.run(tracer)

			records := logs.all()
			require.Len(t, records, 1,
				"%s should be logged without a tracer provider, logged: %v", tt.name, logs.messages())
			assert.Equal(t, tt.wantMsg, records[0].Message)
		})
	}
}
