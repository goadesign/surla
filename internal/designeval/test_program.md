
---

## The test program Surla builds

Surla writes this test module's program, `{{ .Command }}`, each time it generates
the module, and builds only that program when it checks your tests or verifies a
change. The program builds the evaluation suite `{{ .Suite }}` from the generated
tests in `gen/evals/{{ .Suite }}` and from your package `acceptance`. Write only
that package; a `main` package of your own is never built. Every run declares the
environment `{{ .Environment }}`.
{{- if .StoryTests }}

This module's `design/story_tests.go` declares one scenario per finished flow of
the project's Story, each run by the `Hooks` method of the same name.
Regenerating this module keeps those scenarios. The file is generated from the
Story: do not edit it by hand.
{{- end }}

The program calls exactly these names in `acceptance`:

| Name | What it must be |
| --- | --- |
| `Name` | A string constant: the name the program under test is bound as, such as `{{ .Service }}`. |
| `Hooks` | A type whose pointer implements the generated `Hooks` interface, one method per scenario: `func (h *Hooks) <Scenario>(ctx context.Context) (eval.Result, error)`. A missing method does not compile. Listing the tests uses a zero `Hooks`, so it must not need the program under test. |
| `New` | `func New(binary candidate.Artifact) *Hooks`: hooks that run every scenario against the pinned program under test. |
| `Live` | `func Live(ctx context.Context, binary candidate.Artifact, key string, provider model.Provider) (suite.LiveTarget, error)`: starts the program under test for a live copy with a model provider. |
| `LiveThroughGateway` | `func LiveThroughGateway(ctx context.Context, binary candidate.Artifact, key, gatewayAddr string) (suite.LiveTarget, error)`: the same, through a model gateway. |
