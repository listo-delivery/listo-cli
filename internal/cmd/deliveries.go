package cmd

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/listo-delivery/listo-cli/internal/api"
	"github.com/listo-delivery/listo-cli/internal/i18n"
	"github.com/listo-delivery/listo-cli/internal/output"
)

func init() { rootCmd.AddCommand(newDeliveriesCmd()) }

func newDeliveriesCmd() *cobra.Command {
	parent := &cobra.Command{
		Use:     "deliveries",
		Short:   "deliveries.short",
		Long:    "deliveries.long",
		Example: "deliveries.example",
		Args:    cobra.NoArgs,
		RunE:    func(c *cobra.Command, _ []string) error { return c.Help() },
	}

	create := &cobra.Command{
		Use:     "create",
		Short:   "deliveries.create_short",
		Long:    "deliveries.create_long",
		Example: "deliveries.create_example",
		Args:    output.Args(cobra.NoArgs),
		RunE:    runDeliveriesCreate,
	}
	create.Flags().String("data", "", "deliveries.flag_data")
	create.Flags().String("idempotency-key", "", "deliveries.flag_idempotency_key")

	get := &cobra.Command{
		Use:     "get <id>",
		Short:   "deliveries.get_short",
		Long:    "deliveries.get_long",
		Example: "deliveries.get_example",
		Args:    output.Args(cobra.ExactArgs(1)),
		RunE:    runDeliveriesGet,
	}

	cancel := &cobra.Command{
		Use:     "cancel <id>",
		Short:   "deliveries.cancel_short",
		Long:    "deliveries.cancel_long",
		Example: "deliveries.cancel_example",
		Args:    output.Args(cobra.ExactArgs(1)),
		RunE:    runDeliveriesCancel,
	}
	cancel.Flags().String("reason", "", "deliveries.flag_reason")
	cancel.Flags().String("description", "", "deliveries.flag_description")
	_ = cancel.RegisterFlagCompletionFunc("reason", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return api.CancelReasons, cobra.ShellCompDirectiveNoFileComp
	})

	withList(cancel, api.CancelReasons)

	parent.AddCommand(create, get, cancel)
	return parent
}

// --- create ---

// quoteFields are the fields of the developer's order that price a trip. The
// live create copies them onto the quote it shows, so the amount in the
// confirmation is the amount the booked quote charges.
var quoteFields = []string{
	"store_id", "dropoff", "payment_method", "order_value_cents",
	"business_fee_share", "business_fee_cap_cents", "pickup_ready_at", "vehicle", "manifest",
}

// quoteDropoffFields are the parts of a drop-off a quote takes (the quote has
// no recipient).
var quoteDropoffFields = []string{"street", "apt_unit", "lat", "lng"}

func runDeliveriesCreate(c *cobra.Command, _ []string) error {
	rt := Run(c)
	p := rt.Printer
	raw, err := readOrder(c, p)
	if err != nil {
		return err
	}
	var order map[string]json.RawMessage
	if err := json.Unmarshal(raw, &order); err != nil {
		return badOrder(err)
	}
	client, err := rt.Client(c.Context())
	if err != nil {
		return err
	}
	key, _ := c.Flags().GetString("idempotency-key")
	if key == "" {
		key = api.NewIdempotencyKey()
	}

	body := raw
	if rt.Live {
		if err := needsConfirmation(p); err != nil {
			return err
		}
		quote, store, address, err := quoteForLive(c, client, order)
		if err != nil {
			return err
		}
		statement := bookStatement(store, address, quote)
		if p.Yes {
			p.Info(statement)
		} else {
			ok, err := p.Confirm(statement + " " + i18n.T("deliveries.continue"))
			if err != nil {
				return err
			}
			if !ok {
				p.Declined()
				return nil
			}
		}
		order["quote_id"] = mustMarshal(quote.ID)
		body, err = json.Marshal(order)
		if err != nil {
			return err
		}
	}

	p.Infof("deliveries.idempotency_key", i18n.Args{"key": key})
	out, _, err := client.CreateDelivery(c.Context(), body, key)
	if err != nil {
		return err
	}
	if p.JSON {
		return p.PrintRawJSON(out)
	}
	if err := printDelivery(p, out, "deliveries.head_created"); err != nil {
		return err
	}
	p.Infof("deliveries.watch")
	return nil
}

// quoteForLive prices the order about to be booked live, and reads the store's
// name and the drop-off address the confirmation shows.
func quoteForLive(c *cobra.Command, client *api.Client, order map[string]json.RawMessage) (*api.Quote, *api.Store, string, error) {
	var storeID string
	if v, ok := order["store_id"]; ok {
		_ = json.Unmarshal(v, &storeID)
	}
	if storeID == "" {
		return nil, nil, "", output.Usage("deliveries.missing_store").Next("deliveries.next_missing_store")
	}
	var dropoff map[string]json.RawMessage
	if v, ok := order["dropoff"]; ok {
		_ = json.Unmarshal(v, &dropoff)
	}
	var address string
	if v, ok := dropoff["street"]; ok {
		_ = json.Unmarshal(v, &address)
	}
	if address == "" {
		address = i18n.T("deliveries.the_dropoff")
	}

	store, err := client.GetStore(c.Context(), storeID)
	if err != nil {
		return nil, nil, "", err
	}

	q := map[string]json.RawMessage{}
	for _, k := range quoteFields {
		if v, ok := order[k]; ok {
			q[k] = v
		}
	}
	if dropoff != nil {
		d := map[string]json.RawMessage{}
		for _, k := range quoteDropoffFields {
			if v, ok := dropoff[k]; ok {
				d[k] = v
			}
		}
		q["dropoff"] = mustMarshal(d)
	}
	quote, err := client.CreateQuote(c.Context(), q)
	if err != nil {
		return nil, nil, "", err
	}
	return quote, store, address, nil
}

// bookStatement is the sentence that says exactly what a live create will do,
// from the quote the create then books.
func bookStatement(store *api.Store, address string, q *api.Quote) string {
	key := "deliveries.will_book"
	if q.BusinessFeeCents > 0 {
		key = "deliveries.will_book_balance"
	}
	return i18n.T(key, i18n.Args{
		"store":    store.Name,
		"store_id": store.ID,
		"address":  address,
		"fee":      feeText(q.FeeCents, q.Currency),
		"payer":    payer(q.BusinessFeeCents, q.CustomerFeeCents),
	})
}

// --- get ---

func runDeliveriesGet(c *cobra.Command, args []string) error {
	rt := Run(c)
	p := rt.Printer
	client, err := rt.Client(c.Context())
	if err != nil {
		return err
	}
	raw, err := client.GetDelivery(c.Context(), args[0])
	if err != nil {
		return err
	}
	if p.JSON {
		return p.PrintRawJSON(raw)
	}
	return printDelivery(p, raw, "deliveries.head_get")
}

// --- cancel ---

func runDeliveriesCancel(c *cobra.Command, args []string) error {
	rt := Run(c)
	p := rt.Printer
	id := args[0]

	reason, _ := c.Flags().GetString("reason")
	description, _ := c.Flags().GetString("description")
	if reason == "" {
		reason = "other"
	}
	if !slices.Contains(api.CancelReasons, reason) {
		return output.Usage("deliveries.unknown_reason", i18n.Args{"reason": reason}).
			Next("deliveries.next_reasons", i18n.Args{"reasons": strings.Join(api.CancelReasons, ", ")})
	}
	if reason == "other" && description == "" {
		description = i18n.T("deliveries.default_cancel_description")
	}

	client, err := rt.Client(c.Context())
	if err != nil {
		return err
	}
	if rt.Live {
		if err := needsConfirmation(p); err != nil {
			return err
		}
		current, err := client.GetDelivery(c.Context(), id)
		if err != nil {
			return err
		}
		var d api.Delivery
		if err := json.Unmarshal(current, &d); err != nil {
			return err
		}
		statement := i18n.T("deliveries.will_cancel", i18n.Args{"id": d.ID, "code": d.Code, "status": d.Status})
		if p.Yes {
			p.Info(statement)
		} else {
			ok, err := p.Confirm(statement + " " + i18n.T("deliveries.continue"))
			if err != nil {
				return err
			}
			if !ok {
				p.Declined()
				return nil
			}
		}
	}

	raw, err := client.CancelDelivery(c.Context(), id, api.CancelRequest{Reason: reason, Description: description})
	if err != nil {
		return err
	}
	if p.JSON {
		return p.PrintRawJSON(raw)
	}
	return printDelivery(p, raw, "deliveries.head_canceled")
}

// --- shared ---

// needsConfirmation refuses before any call when a live write could not be
// confirmed: no --yes and no terminal to ask on (exit 2, "Add --yes").
func needsConfirmation(p *output.Printer) error {
	if p.Yes || p.InTTY {
		return nil
	}
	return output.Usage("confirm.needs_terminal").WithCode("confirmation_needed").Next("confirm.next_yes")
}

// readOrder returns the developer's own JSON: --data @file.json, --data @- or
// --data '{...}', else standard input when it is not a terminal.
func readOrder(c *cobra.Command, p *output.Printer) (json.RawMessage, error) {
	data, _ := c.Flags().GetString("data")
	var raw []byte
	var err error
	switch {
	case data == "":
		if p.InTTY {
			return nil, output.Usage("deliveries.no_order").Next("deliveries.next_no_order")
		}
		raw, err = io.ReadAll(io.LimitReader(p.In, 1<<20))
	case data == "@-":
		raw, err = io.ReadAll(io.LimitReader(p.In, 1<<20))
	case strings.HasPrefix(data, "@"):
		path := strings.TrimPrefix(data, "@")
		raw, err = os.ReadFile(path)
		if err != nil {
			return nil, output.Usage("deliveries.read_failed", i18n.Args{"path": path, "detail": err.Error()}).
				Next("deliveries.next_no_order").Wrap(err)
		}
	default:
		raw = []byte(data)
	}
	if err != nil {
		return nil, output.Fail("deliveries.read_failed", i18n.Args{"path": "-", "detail": err.Error()}).Wrap(err)
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, output.Usage("deliveries.no_order").Next("deliveries.next_no_order")
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, badOrder(err)
	}
	return json.RawMessage(raw), nil
}

func badOrder(err error) error {
	return output.Usage("deliveries.bad_json", i18n.Args{"detail": err.Error()}).Next("deliveries.next_bad_json").Wrap(err)
}

// printDelivery draws a delivery for a person: the badge and what it is, the fee
// and who pays it, and the tracking link when there is one.
func printDelivery(p *output.Printer, raw json.RawMessage, headKey string) error {
	var d api.Delivery
	if err := json.Unmarshal(raw, &d); err != nil {
		return err
	}
	p.Info(p.Badge() + " " + i18n.T(headKey, i18n.Args{"id": d.ID, "code": d.Code, "status": d.Status}))
	if d.Currency != "" {
		p.Infof("deliveries.fee_line", i18n.Args{
			"fee":   feeText(d.FeeCents, d.Currency),
			"payer": payer(d.BusinessFeeCents, d.CustomerFeeCents),
		})
	}
	if d.TrackingURL != nil && *d.TrackingURL != "" {
		p.Infof("deliveries.tracking", i18n.Args{"url": *d.TrackingURL})
	}
	return nil
}

// payer says who pays a delivery fee, from the two parts the API publishes.
func payer(businessCents, customerCents int64) string {
	switch {
	case businessCents > 0 && customerCents > 0:
		return i18n.T("deliveries.payer_split", i18n.Args{"store": money(businessCents), "customer": money(customerCents)})
	case businessCents > 0:
		return i18n.T("deliveries.payer_store")
	case customerCents > 0:
		return i18n.T("deliveries.payer_customer")
	}
	return i18n.T("deliveries.payer_nobody")
}

func feeText(cents int64, currency string) string {
	if currency == "" {
		currency = "MXN"
	}
	return money(cents) + " " + currency
}

// money writes centavos as $1,234.50.
func money(cents int64) string {
	sign := ""
	if cents < 0 {
		sign, cents = "-", -cents
	}
	whole := strconv.FormatInt(cents/100, 10)
	var grouped []byte
	for i, ch := range []byte(whole) {
		if i > 0 && (len(whole)-i)%3 == 0 {
			grouped = append(grouped, ',')
		}
		grouped = append(grouped, ch)
	}
	frac := strconv.FormatInt(cents%100, 10)
	if len(frac) < 2 {
		frac = "0" + frac
	}
	return sign + "$" + string(grouped) + "." + frac
}

func mustMarshal(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // only ever called with strings, maps of raw JSON
	}
	return b
}
