package orders

import (
	"context"
	"errors"
	"testing"

	gencommerce "github.com/goadesign/surla/examples/orders/gen/commerce"
	genorders "github.com/goadesign/surla/examples/orders/gen/orders"
	genpayments "github.com/goadesign/surla/examples/orders/gen/payments"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	goa "goa.design/goa/v3/pkg"
)

type (
	// fakeOrders serves orders owned by one customer.
	fakeOrders struct {
		// customerID owns every order in byID.
		customerID string
		// byID holds the customer's orders.
		byID map[string]*gencommerce.Order
	}

	// fakeRefunds records refunds instead of running them.
	fakeRefunds struct {
		// got holds every refund requested, in order.
		got []Refund
	}
)

func TestRefundMethod(t *testing.T) {
	cases := []struct {
		name  string
		total int64
		want  string
	}{
		{"well under the limit", 3850, methodStoreCredit},
		{"exactly the limit", 5000, methodStoreCredit},
		{"one cent over", 5001, methodCard},
		{"item under but shipping over", 5400, methodCard},
		{"well over", 8200, methodCard},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, RefundMethod(c.total))
		})
	}
}

func TestRefundTools(t *testing.T) {
	cases := []struct {
		name       string
		tool       func(*Service, context.Context, *genorders.RefundCommand) (*genorders.RefundOutcome, error)
		customerID string
		orderID    string
		wantErr    string
		wantMethod string
	}{
		{"store credit at the limit", (*Service).IssueStoreCredit, "c1", "o-5000", "", methodStoreCredit},
		{"store credit refused over the limit", (*Service).IssueStoreCredit, "c1", "o-5400", "invalid_refund", ""},
		{"card refund over the limit", (*Service).RequestRefund, "c1", "o-5400", "", methodCard},
		{"card refund refused at the limit", (*Service).RequestRefund, "c1", "o-5000", "invalid_refund", ""},
		{"another customer's order", (*Service).IssueStoreCredit, "c2", "o-5000", "invalid_refund", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			refunds := &fakeRefunds{}
			svc := New(newFakeOrders(), refunds, nil, nil)
			out, err := c.tool(svc, context.Background(), &genorders.RefundCommand{
				TenantID: "t1", CustomerID: c.customerID, OrderID: c.orderID,
			})
			if c.wantErr != "" {
				var se *goa.ServiceError
				require.ErrorAs(t, err, &se)
				assert.Equal(t, c.wantErr, se.Name)
				assert.Empty(t, refunds.got, "a refused refund must not reach the payment provider")
				return
			}
			require.NoError(t, err)
			require.Len(t, refunds.got, 1)
			assert.Equal(t, c.wantMethod, refunds.got[0].Method)
			assert.Equal(t, newFakeOrders().byID[c.orderID].TotalCents, refunds.got[0].AmountCents,
				"the full total, including tax and shipping, is refunded")
			assert.Equal(t, "refunded", out.Status)
		})
	}
}

// newFakeOrders returns orders for customer c1 totalling exactly $50.00 and
// $54.00 ($48.00 items plus $6.00 shipping).
func newFakeOrders() *fakeOrders {
	return &fakeOrders{customerID: "c1", byID: map[string]*gencommerce.Order{
		"o-5000": {ID: "o-5000", PlacedAt: "2026-10-01T10:00:00Z", ItemsCents: 4600, TaxCents: 400, TotalCents: 5000},
		"o-5400": {ID: "o-5400", PlacedAt: "2026-10-02T10:00:00Z", ItemsCents: 4800, ShippingCents: 600, TotalCents: 5400},
	}}
}

// ListOrders is unused by these tests.
func (f *fakeOrders) ListOrders(context.Context, *gencommerce.ListOrdersPayload) (*gencommerce.OrderPage, error) {
	return nil, errors.New("not used")
}

// GetOrder returns the order when the customer owns it and the commerce
// not_found error otherwise.
func (f *fakeOrders) GetOrder(_ context.Context, p *gencommerce.GetOrderPayload) (*gencommerce.Order, error) {
	o, ok := f.byID[p.OrderID]
	if !ok || p.CustomerID != f.customerID {
		return nil, gencommerce.MakeNotFound(errors.New("no such order"))
	}
	return o, nil
}

// Refund records the refund and returns a receipt for it.
func (f *fakeRefunds) Refund(_ context.Context, r Refund) (*genpayments.Receipt, error) {
	f.got = append(f.got, r)
	return &genpayments.Receipt{ReceiptID: "r-" + r.OrderID, OrderID: r.OrderID, AmountCents: r.AmountCents, Method: r.Method}, nil
}
