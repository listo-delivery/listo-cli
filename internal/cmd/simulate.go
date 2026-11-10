package cmd

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/listo-delivery/listo-cli/internal/api"
	"github.com/listo-delivery/listo-cli/internal/i18n"
	"github.com/listo-delivery/listo-cli/internal/output"
)

func init() { rootCmd.AddCommand(newSimulateCmd()) }

func newSimulateCmd() *cobra.Command {
	c := &cobra.Command{
		Use:       "simulate <scenario>",
		Short:     "simulate.short",
		Long:      "simulate.long",
		Example:   "simulate.example",
		Args:      output.Args(cobra.ExactArgs(1)),
		ValidArgs: api.SimulateScenarios,
		RunE:      runSimulate,
	}
	c.Flags().String("store", "", "simulate.flag_store")
	withList(c, api.SimulateScenarios)
	return c
}

// The drop-off the quickstart uses: a real address in Guadalajara about 2.4 km
// from the store, inside the area Listo covers (docs-site/en/quickstart.mdx).
// The order is a cash one of $196.00, as the quickstart's is.
var sampleDropoff = map[string]any{
	"name":   "Ana Torres",
	"phone":  "+528112345678",
	"street": "Blvd. Puerta de Hierro 4965, Zapopan, Jalisco 45116",
	"lat":    20.709395,
	"lng":    -103.413393,
}

const sampleOrderValueCents = 19600

func runSimulate(c *cobra.Command, args []string) error {
	rt := Run(c)
	p := rt.Printer
	scenario := args[0]
	if !slices.Contains(api.SimulateScenarios, scenario) {
		return output.Usage("simulate.unknown_scenario", i18n.Args{"scenario": scenario}).
			Next("simulate.next_scenarios", i18n.Args{"scenarios": strings.Join(api.SimulateScenarios, ", ")})
	}
	// The API refuses `simulate` on a live key; say so before calling anything.
	if rt.Live {
		return output.Usage("simulate.refused_live").Next("simulate.next_drop_live")
	}

	client, err := rt.Client(c.Context())
	if err != nil {
		return err
	}
	storeID, _ := c.Flags().GetString("store")
	if storeID == "" {
		err := client.EachStore(c.Context(), func(s api.Store) (bool, error) {
			if s.Test {
				storeID = s.ID
				return false, nil
			}
			return true, nil
		})
		if err != nil {
			return err
		}
		if storeID == "" {
			return output.Fail("simulate.no_store").Next("simulate.next_no_store")
		}
	}

	order, err := json.Marshal(map[string]any{
		"store_id":          storeID,
		"external_id":       "cli-simulate-" + api.NewIdempotencyKey()[:8],
		"payment_method":    "cash",
		"order_value_cents": sampleOrderValueCents,
		"dropoff":           sampleDropoff,
		"simulate":          scenario,
	})
	if err != nil {
		return err
	}
	raw, _, err := client.CreateDelivery(c.Context(), order, "")
	if err != nil {
		return err
	}
	if p.JSON {
		return p.PrintRawJSON(raw)
	}
	var d api.Delivery
	if err := json.Unmarshal(raw, &d); err != nil {
		return err
	}
	p.Infof("simulate.started", i18n.Args{"scenario": scenario, "id": d.ID, "status": d.Status})
	p.Infof("deliveries.watch")
	return nil
}
