// This file declares how callers find the project's saved Change Cards and
// how the views page learns that they changed.
package design

import (
	. "goa.design/goa-ai/dsl"
	. "goa.design/goa/v3/dsl"
)

// LatestVerificationItem summarizes the newest verification run of a Change Card.
var LatestVerificationItem = Type("LatestVerificationItem", func() {
	Description("The most recently accepted Checkpoint 4 verification run of a Change Card.")
	Attribute("run_id", String, "Verification run identifier; get_verification takes it.")
	Attribute("state", String, "running while the run builds or evaluates the candidate; passed or failed when its report's verdict is known; not_completed when it stopped without a report.", func() {
		Enum("running", "passed", "failed", "not_completed")
	})
	Required("run_id", "state")
})

// ChangeListItem summarizes one saved Change Card for finding work.
var ChangeListItem = Type("ChangeListItem", func() {
	Description("One saved Change Card of the project as list_changes reports it.")
	Attribute("change_id", String, "Change Card identifier; get_change takes it.")
	Attribute("summary", String, "Plain-English summary of the current revision.")
	Attribute("status", String, "Checkpoint status of the current revision.", func() {
		Enum("needs_confirmation", "confirmed")
	})
	Attribute("request_revision", String, "Current request_revision of the Change Card.")
	Attribute("latest_verification", LatestVerificationItem, "The Change Card's most recently accepted verification run; absent when none was started.")
	Attribute("saved_at", String, "When the Change Card was last saved. It is not part of request_revision.", func() {
		Format(FormatDateTime)
	})
	Required("change_id", "summary", "status", "request_revision", "saved_at")
})

// ChangeList is the project's change list at one moment.
var ChangeList = Type("ChangeList", func() {
	Description("The project's saved Change Cards, most recently saved first (tie-broken by change_id).")
	Attribute("changes", ArrayOf(ChangeListItem), "Saved Change Cards, most recently saved first.")
	Attribute("version", String, "Opaque value identifying this exact list; it changes whenever any listed value changes.")
	Required("changes", "version")
})

// ChangeListVersion names the change list current when a wait ends.
var ChangeListVersion = Type("ChangeListVersion", func() {
	Description("The version of the change list when the wait ended.")
	Attribute("version", String, "Version of the current change list; it equals since when nothing changed during the wait.")
	Required("version")
})

// changeListMethods exposes the card store owner's project list and its
// waiting read. The waiting read is HTTP-only: only the views page uses it.
func changeListMethods() {
	Method("list_changes", func() {
		Description("List the owner's saved Change Cards for this project, most recently saved first, with each card's summary, status, current request_revision, the state of its most recently accepted verification run, and when it was last saved. The views page opens on this list; agents use it to find a change_id. Reading changes nothing.")
		Result(ChangeList)
		Tool("list_changes", "List this project's Change Cards, most recently saved first, with each card's summary, status, current request_revision, latest verification run state and last save time. Read one card with get_change(change_id).")
		HTTP(func() {
			GET("/changes")
			Response(StatusOK)
		})
	})

	Method("wait_for_changes", func() {
		Description("Return the current change list's version as soon as it differs from since, the version of the list the caller last read, or after a bounded wait of a few seconds when nothing changed. The views page keeps one such read open and calls list_changes again only when the version changed. Each call is traced but never saved in the diagnostics history list_traces reads, so an open page does not push real operations out of view.")
		Payload(func() {
			Attribute("since", String, "version of the change list the caller last read.", func() {
				MinLength(1)
				MaxLength(128)
			})
			Required("since")
		})
		Result(ChangeListVersion)
		HTTP(func() {
			GET("/change-updates")
			Param("since")
			Response(StatusOK)
		})
	})
}
