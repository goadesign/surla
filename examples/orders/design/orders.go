// Package design declares the reference orders platform that Surla builds
// and verifies: a refund agent over a commerce system and a payment provider.
package design

import (
	. "goa.design/goa-ai/dsl"
	. "goa.design/goa/v3/dsl"
)

// JWTAuth secures the public orders API. Customers hold the "orders:chat"
// scope; managers additionally hold "refunds:approve". Operators who may change
// the refund agent's prompts for every tenant hold "prompts:manage".
var JWTAuth = JWTSecurity("jwt", func() {
	Description("Bearer token issued by the merchant's identity provider.")
	Scope("orders:chat", "Talk to the refund assistant about your own orders.")
	Scope("refunds:approve", "Approve or deny refunds that need a manager.")
	Scope("prompts:manage", "Change the refund agent's prompts for every tenant.")
})

var _ = API("orders", func() {
	Title("Orders reference platform")
	Description("Refund assistant used as Surla's reference target under test.")
})

// orderTools are the tools the refund agent can call. Tenant and customer
// identity come from the authenticated request (run labels), never from the
// model.
var orderTools = Toolset("orders", func() {
	Description("Look up the customer's orders and return money for them.")

	Tool("list_orders", "List the customer's orders, newest first.", func() {
		Args(func() {
			Attribute("tenant_id", String, "Merchant tenant.")
			Attribute("customer_id", String, "Customer whose orders to list.")
			Required("tenant_id", "customer_id")
		})
		Return(func() {
			Attribute("orders", ArrayOf(Order), "Orders on this page.")
			Required("orders")
		})
		BindTo("orders", "list_orders")
		Inject("tenant_id", "customer_id")
		BoundedResult(func() {
			ContinueWith("continue_orders", "cursor")
			NextCursor("next_cursor")
		})
	})

	Tool("continue_orders", "Continue listing the customer's orders.", func() {
		Args(func() {
			Attribute("tenant_id", String, "Merchant tenant.")
			Attribute("customer_id", String, "Customer whose orders to list.")
			Attribute("cursor", String, "Cursor of the page to read.")
			Required("tenant_id", "customer_id", "cursor")
		})
		Return(func() {
			Attribute("orders", ArrayOf(Order), "Orders on this page.")
			Required("orders")
		})
		BindTo("orders", "list_orders")
		Inject("tenant_id", "customer_id")
		BoundedResult(func() {
			Cursor("cursor")
			NextCursor("next_cursor")
		})
	})

	Tool("issue_store_credit", "Refund an order whose total, including tax and shipping, is $50.00 or less as instant store credit.", func() {
		Args(func() {
			Attribute("tenant_id", String, "Merchant tenant.")
			Attribute("customer_id", String, "Customer who owns the order.")
			Attribute("order_id", String, "Order to refund.", func() { MinLength(1) })
			Required("tenant_id", "customer_id", "order_id")
		})
		Return(RefundOutcome)
		BindTo("orders", "issue_store_credit")
		Inject("tenant_id", "customer_id")
	})

	Tool("request_refund", "Refund an order whose total, including tax and shipping, is over $50.00. A manager must approve it first.", func() {
		Args(func() {
			Attribute("tenant_id", String, "Merchant tenant.")
			Attribute("customer_id", String, "Customer who owns the order.")
			Attribute("order_id", String, "Order to refund.", func() { MinLength(1) })
			Required("tenant_id", "customer_id", "order_id")
		})
		Return(RefundOutcome)
		BindTo("orders", "request_refund")
		Inject("tenant_id", "customer_id")
		Confirmation(func() {
			Title("Approve refund")
			PromptTemplate(`Refund order {{ .order_id }} to the customer's card?`)
			DeniedResultTemplate(`{"status":"denied","order_id":{{ json .order_id }}}`)
		})
	})
})

// RefundOutcome is what the agent learns after a refund attempt.
var RefundOutcome = Type("RefundOutcome", func() {
	Description("Result of a refund attempt.")
	Attribute("status", String, "What happened to the refund.", func() { Enum("refunded", "denied") })
	Attribute("order_id", String, "Refunded order.")
	Attribute("amount_cents", Int64, "Refunded amount in cents.")
	Attribute("method", String, "How the money was returned.", func() { Enum("store_credit", "card") })
	Attribute("receipt_id", String, "Payment provider receipt.")
	Required("status", "order_id")
})

// RunView is the caller-visible state of one refund conversation turn.
var RunView = Type("RunView", func() {
	Description("State of a refund assistant run.")
	Attribute("run_id", String, "Run identifier.")
	Attribute("status", String, "Run state.", func() { Enum("running", "awaiting_approval", "completed", "failed") })
	Attribute("reply", String, "Assistant reply once the run completes.")
	Attribute("approval_id", String, "Pending approval to decide when status is awaiting_approval.")
	Attribute("approval_prompt", String, "What the manager is asked to approve.")
	Required("run_id", "status")
})

// PromptVersion identifies the prompt text override_prompt accepted.
var PromptVersion = Type("PromptVersion", func() {
	Attribute("prompt_id", String, "Prompt that was replaced.")
	Attribute("version", String, "Version of the accepted text: \"sha256:\" followed by the hex SHA-256 of the template. Model requests that use this text carry this version.")
	Required("prompt_id", "version")
})

var _ = Service("orders", func() {
	Description("Refund assistant for a merchant's customers.")
	Security(JWTAuth, func() { Scope("orders:chat") })
	Error("unauthorized", ErrorResult, "The bearer token is missing or invalid.")
	Error("forbidden", ErrorResult, "The caller may not perform this action.")
	Error("not_found", ErrorResult, "The run or approval does not exist.")
	Error("invalid_refund", ErrorResult, "The refund breaks a business rule.")

	HTTP(func() {
		Response("unauthorized", StatusUnauthorized)
		Response("forbidden", StatusForbidden)
		Response("not_found", StatusNotFound)
		Response("invalid_refund", StatusUnprocessableEntity)
	})

	Method("chat", func() {
		Description("Send a message to the refund assistant and start a run.")
		Payload(func() {
			Token("token", String, "Customer bearer token.")
			Attribute("message", String, "What the customer says.", func() { MinLength(1) })
			Required("token", "message")
		})
		Result(RunView)
		HTTP(func() {
			POST("/chat")
			Response(StatusAccepted)
		})
	})

	Method("run_status", func() {
		Description("Read the state of a run the caller started.")
		Payload(func() {
			Token("token", String, "Customer bearer token.")
			Attribute("run_id", String, "Run to read.", func() { MinLength(1) })
			Required("token", "run_id")
		})
		Result(RunView)
		HTTP(func() {
			GET("/runs/{run_id}")
			Response(StatusOK)
		})
	})

	Method("approve_refund", func() {
		Description("Approve or deny a refund that is waiting for a manager.")
		Security(JWTAuth, func() { Scope("refunds:approve") })
		Payload(func() {
			Token("token", String, "Manager bearer token.")
			Attribute("run_id", String, "Run waiting for approval.", func() { MinLength(1) })
			Attribute("approval_id", String, "Approval being decided, as reported by run_status.", func() { MinLength(1) })
			Attribute("approved", Boolean, "Whether the refund is approved.")
			Required("token", "run_id", "approval_id", "approved")
		})
		Result(RunView)
		HTTP(func() {
			POST("/runs/{run_id}/approval")
			Response(StatusAccepted)
		})
	})

	Method("override_prompt", func() {
		Description("Replace the text of a registered prompt for every tenant served by this orders process. The agent's next model request planned by this process uses the new text. The change lasts until the process restarts, which restores the built-in text; other running orders processes keep their own prompts.")
		Security(JWTAuth, func() { Scope("prompts:manage") })
		Error("unknown_prompt", ErrorResult, "No prompt with this ID is registered.")
		Error("invalid_prompt", ErrorResult, "The prompt template does not render.")
		Payload(func() {
			Token("token", String, "Operator bearer token.")
			Attribute("prompt_id", String, "Registered prompt to replace.", func() {
				MinLength(1)
				Example("orders.refund.system")
			})
			Attribute("template", String, "New prompt text, as a Go text/template. It must render with no template data.", func() { MinLength(1) })
			Required("token", "prompt_id", "template")
		})
		Result(PromptVersion)
		HTTP(func() {
			PUT("/prompts/{prompt_id}")
			Response(StatusOK)
			Response("unknown_prompt", StatusNotFound)
			Response("invalid_prompt", StatusUnprocessableEntity)
		})
	})

	// The following methods back the agent's tools. They have no HTTP
	// transport: only the agent runtime calls them.
	Method("list_orders", func() {
		Description("List one page of the customer's orders.")
		NoSecurity()
		Payload(func() {
			Attribute("tenant_id", String, "Merchant tenant.", func() { MinLength(1) })
			Attribute("customer_id", String, "Customer whose orders to list.", func() { MinLength(1) })
			Attribute("cursor", String, "Cursor of the page to read.")
			Required("tenant_id", "customer_id")
		})
		Result(func() {
			Attribute("orders", ArrayOf(Order), "Orders on this page.")
			Attribute("returned", Int, "Orders on this page.")
			Attribute("total", Int, "Orders the customer has in total.")
			Attribute("truncated", Boolean, "Whether more orders remain.")
			Attribute("next_cursor", String, "Cursor for the next page.")
			Required("orders", "returned", "total", "truncated")
		})
	})

	Method("issue_store_credit", func() {
		Description("Refund an order of $50.00 or less, tax and shipping included, as store credit.")
		NoSecurity()
		Payload(RefundCommand)
		Result(RefundOutcome)
	})

	Method("request_refund", func() {
		Description("Refund an order over $50.00 to the card after manager approval.")
		NoSecurity()
		Payload(RefundCommand)
		Result(RefundOutcome)
	})

	Export(orderTools)

	Agent("refund", "Helps customers get refunds for their orders.", func() {
		Use(orderTools)
		SystemPrompt(`You help a customer get refunds for their own orders.
Use list_orders to find the order the customer means. Refund each order at most once.
An order total includes tax and shipping. If that total is $50.00 or less, use issue_store_credit.
If it is over $50.00, use request_refund; a manager must approve it before the money moves, so tell the customer the refund is waiting for approval.
Never promise a refund before a tool result confirms it. If a tool refuses, explain the reason to the customer in plain words.`, func() {
			Description("Instructions for the refund assistant.")
		})
		RunPolicy(func() {
			DefaultCaps(MaxToolCalls(8), MaxRecoveryTurns(2))
			TimeBudget("2m")
		})
	})
})

// RefundCommand is the server-side refund request built from a tool call. An
// order is refunded at most once, so the order identifies the refund.
var RefundCommand = Type("RefundCommand", func() {
	Description("Refund request with identity supplied by the server.")
	Attribute("tenant_id", String, "Merchant tenant.", func() { MinLength(1) })
	Attribute("customer_id", String, "Customer who owns the order.", func() { MinLength(1) })
	Attribute("order_id", String, "Order to refund.", func() { MinLength(1) })
	Required("tenant_id", "customer_id", "order_id")
})
