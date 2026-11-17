package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"slices"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/kylecannon/gostack/internal/config"
)

const (
	statusCommandName  = "status"
	statusServicesName = "services"

	formatJSON  = "json"
	formatPlain = "plain"
	// formatTable is LocalStack's default format, and prints the same table as
	// plain.
	formatTable = "table"

	// statusTimeout bounds the whole health request, as LocalStack's CLI does.
	statusTimeout = 5 * time.Second
	// maxHealthBody is far more than any health document.
	maxHealthBody = 1 << 20

	exitFailure = 1
	exitUsage   = 2

	statusColumnPadding = 2
)

// errStatusUsage is returned by [parseStatus] for a command line it cannot use.
var errStatusUsage = errors.New("usage: status [services] [--format=json|plain]")

// parseStatus reads the arguments after the program name. It reports whether
// they are a status command at all, so that any other command line takes the
// normal startup path, and returns the output format when they are.
//
// Both names the image installs, gostack and localstack, take the same command
// line: "status services [--format=...]", and "status" alone, which means the
// same.
func parseStatus(args []string) (string, bool, error) {
	if len(args) == 0 || args[0] != statusCommandName {
		return "", false, nil
	}

	rest := args[1:]
	if len(rest) > 0 && rest[0] == statusServicesName {
		rest = rest[1:]
	}

	flags := flag.NewFlagSet(statusCommandName, flag.ContinueOnError)
	// The caller prints the error, so the flag package stays silent.
	flags.SetOutput(io.Discard)

	format := flags.String("format", formatPlain, "output format: json, plain or table")

	err := flags.Parse(rest)
	if err != nil {
		return "", true, fmt.Errorf("%w: %w", errStatusUsage, err)
	}

	if flags.NArg() > 0 {
		return "", true, fmt.Errorf("%w: unexpected argument %q", errStatusUsage, flags.Arg(0))
	}

	switch *format {
	case formatJSON, formatPlain, formatTable:
		return *format, true, nil
	default:
		return "", true, fmt.Errorf("%w: unknown format %q", errStatusUsage, *format)
	}
}

// statusMain runs the status command when args are one, and reports whether
// they were. It reads the environment and makes one HTTP request. It binds no
// port, runs no hook and starts no supervisor.
func statusMain(ctx context.Context, args []string, stdout, stderr io.Writer) (int, bool) {
	format, isStatus, err := parseStatus(args)
	if !isStatus {
		return 0, false
	}

	if err != nil {
		_, _ = fmt.Fprintln(stderr, "gostack:", err)

		return exitUsage, true
	}

	cfg, err := config.Load()
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "gostack: cannot read the configuration:", err)

		return exitFailure, true
	}

	// The server's own default, with the container check, decides the port when
	// GATEWAY_LISTEN is unset. The request always goes to the loopback address.
	port := cfg.Settings().GatewayListen[0].Port
	baseURL := "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(port))

	return runStatus(ctx, baseURL, format, stdout, stderr), true
}

// runStatus asks the health route at baseURL for the service states and prints
// them. It returns 0 on an HTTP 200, and 1 for any other answer or no answer.
func runStatus(ctx context.Context, baseURL, format string, stdout, stderr io.Writer) int {
	services, err := fetchServices(ctx, baseURL)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "gostack:", err)

		return exitFailure
	}

	switch format {
	case formatJSON:
		err = json.NewEncoder(stdout).Encode(services)
	default:
		err = writeServiceTable(stdout, services)
	}

	if err != nil {
		_, _ = fmt.Fprintln(stderr, "gostack: cannot print the service states:", err)

		return exitFailure
	}

	return 0
}

// fetchServices returns the "services" object of GET /_localstack/health, which
// maps each service name to its state.
func fetchServices(ctx context.Context, baseURL string) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, statusTimeout)
	defer cancel()

	url := baseURL + "/_localstack/health"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build the health request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not connect to the health endpoint at %s: %w", url, err)
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the health endpoint at %s answered %s", url, resp.Status)
	}

	var doc struct {
		Services map[string]any `json:"services"`
	}

	err = json.NewDecoder(io.LimitReader(resp.Body, maxHealthBody)).Decode(&doc)
	if err != nil {
		return nil, fmt.Errorf("read the health document from %s: %w", url, err)
	}

	if doc.Services == nil {
		doc.Services = map[string]any{}
	}

	return doc.Services, nil
}

// writeServiceTable prints one line per service, sorted by name.
func writeServiceTable(w io.Writer, services map[string]any) error {
	table := tabwriter.NewWriter(w, 0, 0, statusColumnPadding, ' ', 0)

	_, err := fmt.Fprintln(table, "SERVICE\tSTATE")
	if err != nil {
		return fmt.Errorf("write the table: %w", err)
	}

	for _, name := range slices.Sorted(maps.Keys(services)) {
		_, err = fmt.Fprintf(table, "%s\t%v\n", name, services[name])
		if err != nil {
			return fmt.Errorf("write the table: %w", err)
		}
	}

	err = table.Flush()
	if err != nil {
		return fmt.Errorf("write the table: %w", err)
	}

	return nil
}
