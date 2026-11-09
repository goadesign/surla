// This file regenerates the application or public local eval/ module through
// the server's held workspace and the same publisher used by surla gen.
package controlplane

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	goa "goa.design/goa/v3/pkg"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/designeval"
	"github.com/goadesign/surla/internal/genpublish"
)

// GenerateCode reads the selected local module from the verified active
// workspace, compiles its design in the sandbox, and publishes the generated
// files. It holds the workspace lock and root handles through publication and
// cleanup, so initialization and Close cannot finish while it still writes.
// An error means gen/ was not replaced; after replacement, auxiliary failures
// are warnings in the successful result. It never builds application code or
// runs the module's tests.
func (c *ControlPlane) GenerateCode(
	ctx context.Context, p *gensurla.GenerateCodePayload,
) (res *gensurla.GenerateCodeResult, retErr error) {
	if _, ok := PrincipalFromContext(ctx); !ok {
		return nil, errors.New("authenticated principal is required")
	}
	ctx, span := otel.Tracer("surla/controlplane").Start(ctx, "surla.controlplane.generate_code")
	defer span.End()
	target := "candidate"
	if p.Target != nil {
		target = *p.Target
	}
	span.SetAttributes(attribute.String("surla.generation.target", target))

	c.workspaceMu.Lock()
	defer c.workspaceMu.Unlock()
	genCtx, cancel := context.WithCancel(ctx)
	stopOnServerClose := context.AfterFunc(c.serverCtx, cancel)
	defer func() {
		stopOnServerClose()
		cancel()
	}()
	// A server that already closed must be refused synchronously, even when
	// AfterFunc has not yet delivered its cancellation callback.
	if err := errors.Join(genCtx.Err(), c.serverCtx.Err()); err != nil {
		return nil, failGenerateFault(span, "code generation was canceled; call again on a running server", err)
	}
	if c.activeWorkspaceDir == "" {
		span.AddEvent("generation_refused", trace.WithAttributes(attribute.String("reason", "no_workspace")))
		return nil, gensurla.MakeNotImplemented(errors.New("no workspace is bound; start Surla from the local project directory"))
	}
	moduleDir, workspace, err := openModuleRootWithIdentity(
		c.activeWorkspaceDir, c.boundWorkspaceDev, c.boundWorkspaceIno,
	)
	if err != nil {
		span.AddEvent("generation_refused", trace.WithAttributes(attribute.String("reason", "workspace_changed")))
		return nil, err
	}
	module := workspace
	defer func() {
		var closeErr error
		if module != workspace {
			closeErr = module.Close()
		}
		closeErr = errors.Join(closeErr, workspace.Close())
		if closeErr != nil {
			if res == nil {
				retErr = failGenerateFault(span, "closing the module directory failed", errors.Join(retErr, closeErr))
			} else {
				span.RecordError(closeErr)
				res.Warnings = append(res.Warnings, "gen/ was updated; closing a module directory failed; inspect the server trace for details")
			}
		}
	}()
	if target == "eval" {
		module, err = openGenerationEvalModule(workspace)
		if err != nil {
			// Keep the successfully opened workspace as the only owned root
			// when the child module could not be opened.
			module = workspace
			span.AddEvent("generation_refused", trace.WithAttributes(attribute.String("reason", "invalid_eval_module")))
			return nil, gensurla.MakeUnprocessable(err)
		}
		moduleDir = filepath.Join(moduleDir, "eval")
	}
	if err := reportProgress(genCtx, 0, 3, "Reading the local "+target+" module"); err != nil {
		return nil, failGenerateFault(span, "could not report code-generation progress; call again", err)
	}
	files, err := designeval.ReadModuleSnapshotFromRoot(moduleDir, module)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "read local generation module")
		return nil, gensurla.MakeUnprocessable(fmt.Errorf("could not read the local %s module: %w", target, err))
	}
	if _, ok := files["go.mod"]; !ok {
		return nil, gensurla.MakeUnprocessable(fmt.Errorf("the local %s module has no go.mod; create or restore it before generating code", target))
	}
	if err := reportProgress(genCtx, 1, 3, "Compiling the design and generating code in the sandbox"); err != nil {
		return nil, failGenerateFault(span, "could not report code-generation progress; call again", err)
	}
	generated, err := c.evaluator.Generate(genCtx, files)
	if err != nil {
		message := designeval.GenerationDiagnostic(err) + "\nCorrect any reported local design or dependency problem, or ask the server operator to address compiler setup, storage, or dependency access. Then call generate_code again on the same server."
		return nil, failGenerateFault(span, message, err)
	}
	if err := reportProgress(genCtx, 2, 3, "Publishing generated files"); err != nil {
		return nil, failGenerateFault(span, "could not report code-generation progress; call again", err)
	}
	if err := c.serverCtx.Err(); err != nil {
		return nil, failGenerateFault(span, "code generation was canceled; call again on a running server", err)
	}
	published, err := genpublish.Publish(genCtx, module, generated)
	if err != nil {
		return nil, failGenerateFault(span, "generated files could not be published; inspect the server trace for details", err)
	}
	res = &gensurla.GenerateCodeResult{
		Target: target, FilesWritten: published.WrittenFiles, Warnings: published.Warnings,
	}
	// The service requires both lists even when publication returned no entries.
	if len(res.FilesWritten) == 0 {
		res.FilesWritten = []string{}
	}
	if len(res.Warnings) == 0 {
		res.Warnings = []string{}
	}
	span.AddEvent("generated_files_published", trace.WithAttributes(attribute.Int("files_written", len(res.FilesWritten))))
	if err := reportProgress(genCtx, 3, 3, "Generated files updated; application build and tests still need to run"); err != nil {
		span.RecordError(err)
		res.Warnings = append(res.Warnings, "gen/ was updated; the final progress notification could not be delivered")
	}
	return res, nil
}

// openGenerationEvalModule opens only the public eval/ child of the held
// workspace. The opened handle must identify the same non-symlink directory
// inspected through that parent, so replacement cannot redirect generation.
func openGenerationEvalModule(workspace *os.Root) (*os.Root, error) {
	info, err := workspace.Lstat("eval")
	if err != nil {
		return nil, fmt.Errorf("the local eval/ module is unavailable; create or restore its directory: %w", err)
	}
	if !info.IsDir() {
		return nil, errors.New("the local eval/ module must be a real directory, not a symbolic link or file")
	}
	root, err := workspace.OpenRoot("eval")
	if err != nil {
		return nil, fmt.Errorf("open the local eval/ module: %w", err)
	}
	held, err := root.Stat(".")
	if err != nil || !os.SameFile(info, held) {
		return nil, errors.Join(errors.New("the local eval/ module changed while opening it; restore it and call again"), err, root.Close())
	}
	return root, nil
}

// failGenerateFault records an operation's raw failure on its span and returns
// the owner-selected recovery message as a server fault. The caller chooses
// which diagnostic details to include.
func failGenerateFault(span trace.Span, message string, err error) error {
	span.RecordError(err)
	span.SetStatus(codes.Error, message)
	return goa.Fault("%s", message)
}
