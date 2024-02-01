package main // Define the package name

// Import necessary packages
import (
	"context" // For managing request lifecycles
	"log"     // Logging library
	"net/http" // HTTP client and server implementations
	"os" // Interacting with operating system functionality
	"strconv" // String conversion utilities
	"time" // Time manipulation functions

	// OpenTelemetry packages for instrumentation and trace exporting
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.4.0"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap" // Structured logging package
	"google.golang.org/grpc" // gRPC framework
)

// main function to set up trace providers and start sending requests
func main() {
	shutdown := initTraceProvider() // Initialize the trace provider
	defer shutdown() // Ensure clean shutdown of the trace provider

	continuouslySendRequests() // Start sending requests in a loop
}

// Initializes OTLP exporter and trace provider
func initTraceProvider() func() {
	ctx := context.Background() // Create a new context

	// Get OTLP endpoint from environment variable or use default
	otelAgentAddr, ok := os.LookupEnv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if !ok {
		otelAgentAddr = "0.0.0.0:4317"
	}

	closeTraces := initTracer(ctx, otelAgentAddr) // Initialize the tracer

	return func() { // Return a function to cleanly shutdown the trace exporter
		doneCtx, cancel := context.WithTimeout(ctx, time.Second) // Create a context with timeout for shutdown
		defer cancel() // Ensure the cancel function is called to release resources
		closeTraces(doneCtx) // Close the trace exporter
	}
}

// Initializes and registers a tracer with the global context
func initTracer(ctx context.Context, otelAgentAddr string) func(context.Context) {
	traceClient := otlptracegrpc.NewClient( // Create a new OTLP gRPC client
		otlptracegrpc.WithInsecure(), // Disable TLS for the connection
		otlptracegrpc.WithEndpoint(otelAgentAddr), // Set the OTLP collector endpoint
		otlptracegrpc.WithDialOption(grpc.WithBlock())) // Block until the connection is established
	traceExp, err := otlptrace.New(ctx, traceClient) // Create a new OTLP trace exporter
	handleErr(err, "Failed to create the collector trace exporter") // Handle potential initialization errors

	// Create a new resource with service name and other attributes
	res, err := resource.New(ctx,
		resource.WithFromEnv(), // Pull resource attributes from the environment
		resource.WithProcess(), // Include process information
		resource.WithTelemetrySDK(), // Include telemetry SDK information
		resource.WithHost(), // Include host information
		resource.WithAttributes(
			semconv.ServiceNameKey.String("demo-client"), // Set the service name
		),
	)
	handleErr(err, "failed to create resource") // Handle potential resource creation errors

	bsp := sdktrace.NewBatchSpanProcessor(traceExp) // Create a new batch span processor
	tracerProvider := sdktrace.NewTracerProvider( // Create a new tracer provider
		sdktrace.WithSampler(sdktrace.AlwaysSample()), // Set the sampling strategy to always sample
		sdktrace.WithResource(res), // Set the resource associated with this provider
		sdktrace.WithSpanProcessor(bsp), // Register the span processor with the provider
	)

	otel.SetTextMapPropagator(propagation.TraceContext{}) // Set the global propagator to tracecontext
	otel.SetTracerProvider(tracerProvider) // Register the tracer provider with the OpenTelemetry API

	return func(doneCtx context.Context) { // Return a function to shutdown the trace exporter
		if err := traceExp.Shutdown(doneCtx); err != nil { // Attempt to shutdown the trace exporter
			otel.Handle(err) // Handle any errors that occur during shutdown
		}
	}
}

// Simple error handling function that logs fatal errors
func handleErr(err error, message string) {
	if err != nil {
		log.Fatalf("%s: %v", message, err) // Log the error message and terminate the program
	}
}

// Continuously sends requests, creating a new span for each request
func continuouslySendRequests() {
	tracer := otel.Tracer("demo-client-tracer") // Retrieve a tracer with a specified name

	for { // Infinite loop to continuously send requests
		ctx, span := tracer.Start(context.Background(), "ExecuteRequest") // Start a new span for the request
		makeRequest(ctx) // Send the request
		SuccessfullyFinishedRequestEvent(span) // Record a custom event on the span
		span.End() // End the span
		time.Sleep(time.Duration(1) * time.Second) // Sleep for a second before sending the next request
	}
}

// Sends an HTTP request, instrumented to include tracing information
func makeRequest(ctx context.Context) {

	// Get server endpoint from environment variable or use default
	demoServerAddr, ok := os.LookupEnv("DEMO_SERVER_ENDPOINT")
	if !ok {
		demoServerAddr = "http://0.0.0.0:7080/hello"
	}

	client := http.Client{ // Create a new HTTP client
		Transport: otelhttp.NewTransport(http.DefaultTransport), // Wrap the default transport with OpenTelemetry instrumentation
	}

	req, err := http.NewRequestWithContext(ctx, "GET", demoServerAddr, nil) // Create a new HTTP request with the provided context
	if err != nil {
		handleErr(err, "failed to http request") // Handle any errors in creating the request
	}

	res, err := client.Do(req) // Send the request
	if err != nil {
		panic(err) // Panic if there is an error sending the request
	}
	res.Body.Close() // Close the response body to avoid resource leaks
}

// Records a custom event on the span to indicate successful request completion
func SuccessfullyFinishedRequestEvent(span trace.Span, opts ...trace.EventOption) {
	opts = append(opts, trace.WithAttributes(attribute.String("someKey", "someValue"))) // Add custom attributes to the event
	span.AddEvent("successfully finished request operation", opts...) // Add the custom event to the span
}

// Enhances a zap logger with trace and span IDs for better correlation between logs and traces
func WithCorrelation(span trace.Span, log *zap.Logger) *zap.Logger {
	return log.With(
		zap.String("span_id", convertTraceID(span.SpanContext().SpanID().String())), // Add the span ID to the log
		zap.String("trace_id", convertTraceID(span.SpanContext().TraceID().String())), // Add the trace ID to the log
	)
}

// Converts a trace ID from hexadecimal to decimal format
func convertTraceID(id string) string {
	if len(id) < 16 { // Check if the ID is shorter than expected
		return "" // Return an empty string if the ID is invalid
	}
	if len(id) > 16 { // If the ID is longer than 16 characters
		id = id[16:] // Use the last 16 characters
	}
	intValue, err := strconv.ParseUint(id, 16, 64) // Convert the hexadecimal string to a uint64
	if err != nil {
		return "" // Return an empty string if there is a conversion error
	}
	return strconv.FormatUint(intValue, 10) // Convert the uint64 value to a decimal string
}
