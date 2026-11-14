// This file names the tests a Story requires without generating anything.
package designeval

// StoryScenarios returns, in declaration order, the scenario and hook method
// the Story table (StoryTests) declares for each complete flow of stories, and
// the incomplete flows it leaves out. A flow's key maps to its scenario with
// Goa's codegen.SnakeCase and to its hook method with codegen.Goify(scenario,
// true); the scenario's description is the flow's title, or its key when the
// title is blank. It applies the same rule and the same collision check as
// StoryTests, because both call mapStoryFlows, so a caller that shows the
// required tests names exactly what generation will require. It runs nothing
// and reads no files. An error names two complete flows that map to the same
// scenario or hook, or a flow whose key maps to an empty name; StoryTests
// refuses the same stories.
func StoryScenarios(stories []StorySummary) ([]StoryScenario, []StoryDraft, error) {
	included, excluded, _, err := mapStoryFlows(stories)
	return included, excluded, err
}
