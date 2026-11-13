// This file names the tests a Story requires without generating anything.
package designeval

// StoryScenarios returns, in declaration order, the scenario and hook method
// GenerateWithStories adds for each complete flow of stories, and the
// incomplete flows it leaves out. It applies the same naming rule and the same
// collision check as GenerateWithStories, because both call mapStoryFlows, so
// a caller that shows the required tests names exactly what generation will
// require. It runs nothing and reads no files. An error names two complete
// flows that map to the same scenario or hook, or a flow whose key maps to an
// empty name; GenerateWithStories refuses the same stories.
func StoryScenarios(stories []StorySummary) ([]StoryScenario, []StoryDraft, error) {
	included, excluded, _, err := mapStoryFlows(stories)
	return included, excluded, err
}
