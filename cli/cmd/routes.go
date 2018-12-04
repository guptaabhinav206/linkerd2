package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/linkerd/linkerd2/controller/api/util"
	pb "github.com/linkerd/linkerd2/controller/gen/public"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

type routesOptions struct {
	statOptionsBase
	toService          string
	toServiceNamespace string
	toAll              bool
	toExternal         string
}

const defaultRoute = "[UNKNOWN]"

func newRoutesOptions() *routesOptions {
	return &routesOptions{
		statOptionsBase:    *newStatOptionsBase(),
		toService:          "",
		toServiceNamespace: "",
		toAll:              false,
		toExternal:         "",
	}
}

func newCmdRoutes() *cobra.Command {
	options := newRoutesOptions()

	cmd := &cobra.Command{
		Use:   "routes [flags] (RESOURCES)",
		Short: "Display route stats",
		Long: `Display route stats.

This command will only display traffic which is sent to a service that has a Service Profile defined.`,
		Example: `  # Routes for the webapp service in the test namespace.
  linkerd routes service/webapp -n test

  # Routes for calls from from the traffic deployment to the webapp service in the test namespace.
  linkerd routes deploy/traffic -n test --to-service webapp`,
		Args:      cobra.ExactArgs(1),
		ValidArgs: util.ValidTargets,
		RunE: func(cmd *cobra.Command, args []string) error {
			req, err := buildTopRoutesRequest(args[0], options)
			if err != nil {
				return fmt.Errorf("error creating metrics request while making routes request: %v", err)
			}

			output, err := requestRouteStatsFromAPI(validatedPublicAPIClient(time.Time{}), req, options)
			if err != nil {
				return err
			}

			_, err = fmt.Print(output)

			return err
		},
	}

	cmd.PersistentFlags().StringVarP(&options.namespace, "namespace", "n", options.namespace, "Namespace of the specified resource")
	cmd.PersistentFlags().StringVarP(&options.timeWindow, "time-window", "t", options.timeWindow, "Stat window (for example: \"10s\", \"1m\", \"10m\", \"1h\")")
	cmd.PersistentFlags().StringVar(&options.toService, "to-service", options.toService, "If present, shows outbound stats to the specified service name")
	cmd.PersistentFlags().StringVar(&options.toServiceNamespace, "to-service-namespace", options.toServiceNamespace, "Sets the namespace used to lookup the \"--to-service\" resource; by default the current \"--namespace\" is used")
	cmd.PersistentFlags().BoolVar(&options.toAll, "to-all", options.toAll, "If present, shows outbound stats to all services")
	cmd.PersistentFlags().StringVar(&options.toExternal, "to-external", options.toExternal, "If present, shows outbound stats to the specified external DNS name")
	cmd.PersistentFlags().StringVarP(&options.outputFormat, "output", "o", options.outputFormat, "Output format; currently only \"table\" (default) and \"json\" are supported")

	return cmd
}

func requestRouteStatsFromAPI(client pb.ApiClient, req *pb.TopRoutesRequest, options *routesOptions) (string, error) {
	resp, err := client.TopRoutes(context.Background(), req)
	if err != nil {
		return "", fmt.Errorf("TopRoutes API error: %v", err)
	}
	if e := resp.GetError(); e != nil {
		return "", fmt.Errorf("TopRoutes API response error: %v", e.Error)
	}

	return renderRouteStats(resp, options), nil
}

func renderRouteStats(resp *pb.TopRoutesResponse, options *routesOptions) string {
	var buffer bytes.Buffer
	w := tabwriter.NewWriter(&buffer, 0, 0, padding, ' ', tabwriter.AlignRight)
	writeRouteStatsToBuffer(resp, w, options)
	w.Flush()

	return renderStats(buffer, &options.statOptionsBase)
}

func writeRouteStatsToBuffer(resp *pb.TopRoutesResponse, w *tabwriter.Writer, options *routesOptions) {
	table := make(map[string]*rowStats)

	for _, r := range resp.GetRoutes().Rows {
		if r.Stats != nil {
			table[r.Route] = &rowStats{
				dst:         r.GetDst(),
				requestRate: util.GetRequestRate(r.Stats, r.TimeWindow),
				successRate: util.GetSuccessRate(r.Stats),
				tlsPercent:  util.GetPercentTls(r.Stats),
				latencyP50:  r.Stats.LatencyMsP50,
				latencyP95:  r.Stats.LatencyMsP95,
				latencyP99:  r.Stats.LatencyMsP99,
			}
		}
	}

	switch options.outputFormat {
	case "table", "":
		if len(table) == 0 {
			fmt.Fprintln(os.Stderr, "No traffic found.  Does the service have a service profile?  You can create one with the `linkerd profile` command.")
			os.Exit(0)
		}
		printRouteTable(table, w, options)
	case "json":
		printRouteJson(table, w)
	}
}

func printRouteTable(stats map[string]*rowStats, w *tabwriter.Writer, options *routesOptions) {
	sortedRoutes, routeWidth := sortRoutes(stats)
	// template for left-aligning the route column
	routeTemplate := fmt.Sprintf("%%-%ds", routeWidth)

	headers := []string{
		fmt.Sprintf(routeTemplate, "ROUTE"),
		"SERVICE",
		"SUCCESS",
		"RPS",
		"LATENCY_P50",
		"LATENCY_P95",
		"LATENCY_P99",
		"TLS\t", // trailing \t is required to format last column
	}

	fmt.Fprintln(w, strings.Join(headers, "\t"))

	templateString := routeTemplate + "\t%s\t%.2f%%\t%.1frps\t%dms\t%dms\t%dms\t%.f%%\t\n"
	templateStringEmpty := "%s\t-\t-\t-\t-\t-\t-\t-\t\n"

	for _, route := range sortedRoutes {
		if row, ok := stats[route]; ok {
			if route == "" {
				route = defaultRoute
			}
			fmt.Fprintf(w, templateString, route,
				row.dst,
				row.successRate*100,
				row.requestRate,
				row.latencyP50,
				row.latencyP95,
				row.latencyP99,
				row.tlsPercent*100,
			)
		} else {
			fmt.Fprintf(w, templateStringEmpty, route)
		}
	}
}

// Using pointers there where the value is NA and the corresponding json is null
type jsonRouteStats struct {
	Route        string   `json:"route"`
	Service      string   `json:"service"`
	Success      *float64 `json:"success"`
	Rps          *float64 `json:"rps"`
	LatencyMSp50 *uint64  `json:"latency_ms_p50"`
	LatencyMSp95 *uint64  `json:"latency_ms_p95"`
	LatencyMSp99 *uint64  `json:"latency_ms_p99"`
	Tls          *float64 `json:"tls"`
}

func printRouteJson(stats map[string]*rowStats, w *tabwriter.Writer) {
	// avoid nil initialization so that if there are not stats it gets marshalled as an empty array vs null
	entries := []*jsonRouteStats{}
	sortedRoutes, _ := sortRoutes(stats)
	for _, route := range sortedRoutes {
		entry := &jsonRouteStats{
			Route: route,
		}
		if route == "" {
			entry.Route = "[UNKNOWN]"
		}
		if row, ok := stats[route]; ok {
			entry.Service = row.dst
			entry.Success = &row.successRate
			entry.Rps = &row.requestRate
			entry.LatencyMSp50 = &row.latencyP50
			entry.LatencyMSp95 = &row.latencyP95
			entry.LatencyMSp99 = &row.latencyP99
			entry.Tls = &stats[route].tlsPercent
		}
		entries = append(entries, entry)
	}
	b, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		log.Error(err.Error())
		return
	}
	fmt.Fprintf(w, "%s\n", b)
}

func buildTopRoutesRequest(resource string, options *routesOptions) (*pb.TopRoutesRequest, error) {
	err := options.validateOutputFormat()
	if err != nil {
		return nil, err
	}

	target, err := util.BuildResource(options.namespace, resource)
	if err != nil {
		return nil, err
	}

	requestParams := util.TopRoutesRequestParams{
		StatsBaseRequestParams: util.StatsBaseRequestParams{
			TimeWindow:   options.timeWindow,
			ResourceName: target.Name,
			ResourceType: target.Type,
			Namespace:    options.namespace,
		},
	}

	var toService string
	if options.toService != "" {
		toNamespace := options.toServiceNamespace
		if toNamespace == "" {
			toNamespace = options.namespace
		}
		toService = fmt.Sprintf("%s.%s.svc.cluster.local", options.toService, toNamespace)
	}

	if options.toExternal != "" {
		if toService != "" {
			return nil, errors.New("\"--to-external\" and \"--to-service\" are mutually exclusive")
		}
		toService = options.toExternal
	}

	if toService != "" && options.toAll {
		return nil, errors.New("\"--to-all\" cannot be used with \"--to-service\" or \"--to-external\"")
	}

	if toService != "" {
		requestParams.ToService = toService
	}
	if options.toAll {
		requestParams.ToAll = true
	}

	return util.BuildTopRoutesRequest(requestParams)
}

// returns a sorted list of keys and the length of the longest key
func sortRoutes(stats map[string]*rowStats) ([]string, int) {
	var sortedRoutes []string
	maxLength := len(defaultRoute)
	hasDefaultRoute := false
	for key := range stats {
		if key == "" {
			hasDefaultRoute = true
			continue
		}
		sortedRoutes = append(sortedRoutes, key)
		if len(key) > maxLength {
			maxLength = len(key)
		}
	}
	sort.Strings(sortedRoutes)
	if hasDefaultRoute {
		sortedRoutes = append(sortedRoutes, "")
	}
	return sortedRoutes, maxLength
}