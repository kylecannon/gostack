package main

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/config"
	"github.com/kylecannon/gostack/internal/persist"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// probeTimeout bounds one call of an operation with an empty request.
const probeTimeout = 10 * time.Second

// answers501 calls the method of handler for operation with an empty input and
// reports whether it answered the generated 501 (awserr.NotImplemented). A
// panic, which an empty request can cause in a method that expects a decoded
// field, comes back as the error: the method has code of its own, but the probe
// cannot say what it answers, so the test fails on it.
func answers501(handler reflect.Value, method reflect.Method, name, operation string) (is501 bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()

	rc := &request.Context{
		Service:   name,
		Operation: operation,
		AccountID: "000000000000",
		Region:    "us-east-1",
		RequestID: "00000000-0000-4000-8000-000000000000",
		Request:   httptest.NewRequestWithContext(ctx, http.MethodPost, "/", nil),
	}
	input := reflect.New(method.Type.In(2).Elem())

	defer func() {
		if recovered := recover(); recovered != nil {
			is501, err = false, fmt.Errorf("panicked: %v", recovered)
		}
	}()

	results := method.Func.Call([]reflect.Value{handler, reflect.ValueOf(request.With(ctx, rc)), input})
	returned, _ := reflect.TypeAssert[error](results[len(results)-1])

	var awsErr *awserr.Error

	return errors.As(returned, &awsErr) && awsErr.StatusCode == http.StatusNotImplemented, nil
}

// Test_serviceTable_Capabilities_noCountedOperationAnswers501 holds, for every
// registered service, what GET /_gostack/services and docs/features.md say:
// an operation counted as implemented does not answer the generated 501 to an
// empty request, and an operation a service declares a stub answers it. The
// first catches a method that checks its request and then delegates to the
// embedded Unimplemented; the second a stub whose operation gained behavior
// and still sits in Stubs.
func Test_serviceTable_Capabilities_noCountedOperationAnswers501(t *testing.T) {
	cfg, err := config.Load()
	require.NoError(t, err)

	regs := newProviders(cfg, func(string) {}).regs
	table := &serviceTable{regs: regs, eng: persist.New(persist.Config{})}
	capabilities := table.Capabilities()
	require.Len(t, capabilities, len(regs))

	for i, capability := range capabilities {
		reg := regs[i]

		t.Run(capability.Name, func(t *testing.T) {
			require.False(t, capability.Unknown)

			handler := reflect.ValueOf(reg.Handler)

			probe := func(operation string) bool {
				method, found := reg.Metadata.OperationMethod(handler.Type(), operation)
				require.True(t, found, "%s has a method for %s", capability.Name, operation)

				is501, probeErr := answers501(handler, method, capability.Name, operation)
				require.NoError(t, probeErr, "%s %s", capability.Name, operation)

				return is501
			}

			for _, operation := range capability.Implemented {
				assert.False(t, probe(operation),
					"%s counts %s as implemented, and it answers the generated 501: delete the method, or list it in Stubs",
					capability.Name, operation)
			}

			stubber, isStubber := reg.Handler.(service.Stubber)
			if !isStubber {
				return
			}

			for _, operation := range stubber.Stubs() {
				assert.Contains(t, reg.Metadata.Operations, operation, "%s lists a stub that is not an operation", capability.Name)
				assert.NotContains(t, capability.Implemented, operation, "%s counts the stub %s", capability.Name, operation)
				assert.True(t, probe(operation),
					"%s lists %s as a stub, and it no longer answers the generated 501: remove it from Stubs",
					capability.Name, operation)
			}
		})
	}
}

// Test_serviceTable_Capabilities_stubsAreNotCounted pins the two Secrets
// Manager operations that check the SecretId and then answer 501: they are
// declared, so a reading of the methods alone would count them, and they are
// in the 23 of the API but not in the 21 implemented.
func Test_serviceTable_Capabilities_stubsAreNotCounted(t *testing.T) {
	cfg, err := config.Load()
	require.NoError(t, err)

	table := &serviceTable{regs: newProviders(cfg, func(string) {}).regs, eng: persist.New(persist.Config{})}

	for _, capability := range table.Capabilities() {
		if capability.Name != "secretsmanager" {
			continue
		}

		require.False(t, capability.Unknown)
		require.Equal(t, 23, capability.Total)
		require.Len(t, capability.Implemented, 21)
		require.NotContains(t, capability.Implemented, "ValidateResourcePolicy")
		require.NotContains(t, capability.Implemented, "StopReplicationToReplica")

		return
	}

	require.Fail(t, "secretsmanager is not registered")
}

// Test_serviceStubs_matchTheUnimplementedCalls holds, for every service
// directory under internal/service, that the operations its code hands to the
// generated Unimplemented (a call of the form x.Unimplemented.Op(...)) are
// exactly the operations its Provider names in Stubs. A method that gains such
// a call without a Stubs entry would be counted as implemented by a reading of
// the methods alone, and an entry whose call was removed would hide an
// operation that works. The probe above would also catch both, but only for
// the operations it can call with an empty request; this reads the source.
func Test_serviceStubs_matchTheUnimplementedCalls(t *testing.T) {
	cfg, err := config.Load()
	require.NoError(t, err)

	declared := map[string][]string{}

	for _, reg := range newProviders(cfg, func(string) {}).regs {
		handlerType := reflect.TypeOf(reg.Handler)
		for handlerType.Kind() == reflect.Pointer {
			handlerType = handlerType.Elem()
		}

		stubber, isStubber := reg.Handler.(service.Stubber)
		if !isStubber {
			continue
		}

		dir := path.Base(handlerType.PkgPath())
		declared[dir] = append(declared[dir], stubber.Stubs()...)
	}

	called := map[string][]string{}
	root := filepath.Join("..", "..", "internal", "service")

	err = filepath.WalkDir(root, func(file string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		if entry.IsDir() || !strings.HasSuffix(file, ".go") || strings.HasSuffix(file, "_test.go") {
			return nil
		}

		parsed, parseErr := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if parseErr != nil {
			return fmt.Errorf("parse %s: %w", file, parseErr)
		}

		rel, relErr := filepath.Rel(root, file)
		if relErr != nil {
			return fmt.Errorf("relative path of %s: %w", file, relErr)
		}

		dir, _, _ := strings.Cut(filepath.ToSlash(rel), "/")

		ast.Inspect(parsed, func(node ast.Node) bool {
			if operation, found := unimplementedCall(node); found {
				called[dir] = append(called[dir], operation)
			}

			return true
		})

		return nil
	})
	require.NoError(t, err)

	require.NotEmpty(t, called, "no call of Unimplemented found: the scan is not reading the services")

	dirs := map[string]bool{}
	for dir := range called {
		dirs[dir] = true
	}

	for dir := range declared {
		dirs[dir] = true
	}

	for dir := range dirs {
		assert.ElementsMatch(t, called[dir], declared[dir],
			"%s: the operations its code hands to Unimplemented (first list) differ from its Stubs (second)", dir)
	}
}

// unimplementedCall reports the operation of a call of the form
// x.Unimplemented.Operation(...), which is how a method answers 501 through
// the generated struct it embeds.
func unimplementedCall(node ast.Node) (operation string, found bool) {
	call, isCall := node.(*ast.CallExpr)
	if !isCall {
		return "", false
	}

	method, isSelector := call.Fun.(*ast.SelectorExpr)
	if !isSelector {
		return "", false
	}

	embedded, isEmbedded := method.X.(*ast.SelectorExpr)
	if !isEmbedded || embedded.Sel.Name != "Unimplemented" {
		return "", false
	}

	return method.Sel.Name, true
}
