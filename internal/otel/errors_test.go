package otel

// var ErrBoom = errors.New("boom")

// func TestRecordAndLogError(t *testing.T) {
// 	buf := logtest.Capture(t)

// 	recorder := tracetest.NewSpanRecorder()
// 	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
// 	t.Cleanup(func() { require.NoError(t, provider.Shutdown(context.Background())) })

// 	tracer := provider.Tracer("test")
// 	require.NotNil(t, tracer)
// 	ctx, span := tracer.Start(context.Background(), "op")
// 	sentinel := ErrBoom

// 	RecordAndLogError(ctx, span, "widget exploded", sentinel, slog.String(otelkeys.Owner, "acme"))
// 	span.End()

// 	ended := recorder.Ended()
// 	require.Len(t, ended, 1)
// 	// The span carries the error status with msg as the description and an
// 	// exception event recording the error itself.
// 	require.Equal(t, codes.Error, ended[0].Status().Code)
// 	require.Equal(t, "widget exploded", ended[0].Status().Description)
// 	require.Len(t, ended[0].Events(), 1)

// 	// The same message is logged at Error level with the error and any extra
// 	// attrs attached.
// 	line := buf.String()
// 	require.Contains(t, line, "level=ERROR")
// 	require.Contains(t, line, `msg="widget exploded"`)
// 	require.Contains(t, line, "boom")
// 	require.Contains(t, line, "acme")
// }

// func TestRecoverPanic(t *testing.T) {
// 	buf := logtest.Capture(t)

// 	recorder := tracetest.NewSpanRecorder()
// 	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
// 	t.Cleanup(func() { require.NoError(t, provider.Shutdown(context.Background())) })
// 	tracer := provider.Tracer("test")
// 	require.NotNil(t, tracer)
// 	ctx, span := tracer.Start(context.Background(), "op")

// 	done := make(chan struct{})
// 	go func() {
// 		defer close(done)
// 		defer span.End()
// 		defer RecoverPanic(ctx, span, "panic in worker")
// 		panic("kaboom")
// 	}()
// 	<-done // the process is still alive, which is the point

// 	ended := recorder.Ended()
// 	require.Len(t, ended, 1)
// 	require.Equal(t, codes.Error, ended[0].Status().Code)
// 	require.Equal(t, "panic in worker", ended[0].Status().Description)
// 	require.Len(t, ended[0].Events(), 1)

// 	line := buf.String()
// 	require.Contains(t, line, "level=ERROR")
// 	require.Contains(t, line, `msg="panic in worker"`)
// 	require.Contains(t, line, "panic: kaboom")
// 	require.Contains(t, line, "exception.stacktrace")

// 	// A nil span is valid both with and without a panic.
// 	func() { defer RecoverPanic(ctx, nil, "unused") }()
// 	func() {
// 		defer RecoverPanic(ctx, nil, "panic without span")
// 		panic("nil span")
// 	}()
// 	require.Contains(t, buf.String(), `msg="panic without span"`)
// 	require.Contains(t, buf.String(), "panic: nil span")
// }

// func TestReportPanic(t *testing.T) {
// 	buf := logtest.Capture(t)
// 	var sent int

// 	// The panic is reported once, logged, and then reaches the caller unchanged.
// 	require.PanicsWithValue(t, "kaboom", func() {
// 		defer ReportPanic(ctx, nil, "panic in task")
// 		panic("kaboom")
// 	})
// 	require.Equal(t, 1, sent)
// 	require.Contains(t, buf.String(), `msg="panic in task"`)
// 	require.Contains(t, buf.String(), "panic: kaboom")

// 	// No panic, no effect.
// 	require.NotPanics(t, func() { defer ReportPanic(context.Background(), nil, "unused") })
// }

// func TestCaptureError(t *testing.T) {
// 	var sent int

// 	CaptureError(ctx, nil)
// 	CaptureError(ctx, fmt.Errorf("deliver reply: %w", context.Canceled))
// 	require.Zero(t, sent, "nil and cancellation errors are not reported")

// 	CaptureError(ctx, fmt.Errorf("deliver reply: %w", ErrBoom))
// 	require.Equal(t, 1, sent)

// 	// A task hitting its own budget is a failure worth reporting.
// 	CaptureError(ctx, fmt.Errorf("deliver reply: %w", context.DeadlineExceeded))
// 	require.Equal(t, 2, sent)
// }

// func TestReportFatal(t *testing.T) {
// 	var sent int
// 	client, err := sentry.NewClient(sentry.ClientOptions{
// 		BeforeSend: func(event *sentry.Event, _ *sentry.EventHint) *sentry.Event {
// 			sent++
// 			return nil
// 		},
// 	})
// 	require.NoError(t, err)
// 	ctx := sentry.SetHubOnContext(context.Background(), sentry.NewHub(client, sentry.NewScope()))

// 	ReportFatal(ctx, context.Canceled)
// 	require.Zero(t, sent, "a cancelled shutdown is not a failure")

// 	ReportFatal(ctx, fmt.Errorf("failed to set up database: %w", ErrBoom))
// 	require.Equal(t, 1, sent)
// }
