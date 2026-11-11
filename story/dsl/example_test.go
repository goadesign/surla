package dsl_test

import (
	"fmt"

	. "github.com/goadesign/surla/story/dsl"
	"github.com/goadesign/surla/story/expr"
	"goa.design/goa/v3/eval"
)

func ExampleStory() {
	// Tests reset the process-wide context; ordinary design packages rely on
	// the Story root registered automatically when the package is imported.
	eval.Reset()
	expr.Root = new(expr.RootExpr)
	if err := eval.Register(expr.Root); err != nil {
		panic(err)
	}
	Story("Refunds", func() {
		Actor("customer", "Customer")
		Actor("manager", "Support manager")
		Flow("refund-over-limit", "Refund above the limit", func() {
			StartsWith("request-refund", "customer", "Requests a refund")
			Step("approve-refund", "manager", "Approves the refund")
			Outcome("refund-paid", "customer", "Receives the refund")
		})
		Flow("refund-without-receipt", "Refund without a receipt", func() {
			StartsWith("request-refund", "customer", "Requests a refund", func() {
				Question("proof-of-purchase", "Which proof of purchase is acceptable?", func() {
					Option("Order number")
					Option("Payment record")
				})
			})
		})
	})
	if err := eval.RunDSL(); err != nil {
		panic(err)
	}
	for _, flow := range expr.Root.Stories[0].Flows {
		fmt.Printf("%s: complete=%t\n", flow.Key, flow.Complete())
	}
	// Output:
	// refund-over-limit: complete=true
	// refund-without-receipt: complete=false
}
