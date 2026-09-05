# Copyright Built On Envoy
# SPDX-License-Identifier: Apache-2.0
# The full text of the Apache license is available in the LICENSE file at
# the root of the repo.

# Goal: provide one repository-wide check that runs every applicable local CI
# check, reports every failure, and remains readable by hiding successful
# command output. Extension checks are separate targets so `make -j check` can
# run independent extensions concurrently while project phases stay serial.
# Keep-going reports all failures instead of stopping at the first one.
MAKEFLAGS += -k
MAKEFLAGS += --no-print-directory
# Recursive project targets have their own prerequisites; keep those serial to
# avoid overlapping generators and other shared build outputs.
SERIAL_MAKE := $(MAKE) -j1
# Match the authoritative `go` directive in the root module on every invocation.
GO_VERSION := $(shell sed -ne 's/^go //p' go.mod)
override GOTOOLCHAIN := go$(GO_VERSION)
export GOTOOLCHAIN
EXTENSION_NAMES := $(filter-out composer,$(patsubst extensions/%/manifest.yaml,%,$(wildcard extensions/*/manifest.yaml)))
EXTENSION_CHECK_TARGETS := $(addprefix extension-check-,$(EXTENSION_NAMES))

.PHONY: check prepare cli-checks ui-checks website-check composer-check extensions-check extensions-e2e-check extensions-e2e-tests $(EXTENSION_CHECK_TARGETS)

# Keep successful command output quiet while replaying a failed command's complete log.
check:
	@status=0; summary_file=$$(mktemp "$${TMPDIR:-/tmp}/boe-check-summary.XXXXXX") || exit 1; export CHECK_SUMMARY_FILE=$$summary_file; \
	run_check() { label=$$1; shift; printf 'START: %s\n' "$$label"; log=$$(mktemp "$${TMPDIR:-/tmp}/boe-check.XXXXXX") || { printf 'FAIL: %s\n' "$$label" >&2; printf 'FAIL\t%s\n' "$$label" >>"$$CHECK_SUMMARY_FILE"; status=1; return 1; }; if "$$@" >"$$log" 2>&1; then rm -f "$$log"; printf 'PASS: %s\n' "$$label"; printf 'PASS\t%s\n' "$$label" >>"$$CHECK_SUMMARY_FILE"; else cat "$$log" >&2; rm -f "$$log"; printf 'FAIL: %s\n' "$$label" >&2; printf 'FAIL\t%s\n' "$$label" >>"$$CHECK_SUMMARY_FILE"; status=1; return 1; fi; }; \
	run_phase() { label=$$1; shift; printf 'START: %s\n' "$$label"; "$$@"; rc=$$?; if [ $$rc -eq 0 ]; then printf 'PASS: %s\n' "$$label"; printf 'PASS\t%s\n' "$$label" >>"$$CHECK_SUMMARY_FILE"; else printf 'FAIL: %s\n' "$$label" >&2; printf 'FAIL\t%s\n' "$$label" >>"$$CHECK_SUMMARY_FILE"; status=1; fi; return $$rc; }; \
	run_check prepare $(SERIAL_MAKE) prepare; \
	run_phase cli-checks $(SERIAL_MAKE) cli-checks; \
	run_phase website-check $(SERIAL_MAKE) website-check; \
	run_phase composer-check $(SERIAL_MAKE) composer-check; \
	run_phase extensions-check $(MAKE) extensions-check; \
	run_phase ui-checks $(SERIAL_MAKE) ui-checks; \
	run_phase extensions-e2e-check $(SERIAL_MAKE) extensions-e2e-check; \
	run_phase extensions-e2e-tests $(SERIAL_MAKE) extensions-e2e-tests; \
	run_check cli/check $(SERIAL_MAKE) -C cli check; \
	run_check ui/check $(SERIAL_MAKE) -C ui check; \
	pass_file=$$(mktemp "$${TMPDIR:-/tmp}/boe-check-passed.XXXXXX"); fail_file=$$(mktemp "$${TMPDIR:-/tmp}/boe-check-failed.XXXXXX"); \
	awk -F '\t' '$$1 == "PASS" { print $$2 }' "$$summary_file" | sort -u >"$$pass_file"; \
	awk -F '\t' '$$1 == "FAIL" { print $$2 }' "$$summary_file" | sort -u >"$$fail_file"; \
	printf '\nSuccessful checks:\n'; if [ -s "$$pass_file" ]; then sed 's/^/  /' "$$pass_file"; else printf '  (none)\n'; fi; \
	printf '\nFailed checks:\n'; if [ -s "$$fail_file" ]; then sed 's/^/  /' "$$fail_file"; else printf '  (none)\n'; fi; \
	rm -f "$$summary_file" "$$pass_file" "$$fail_file"; exit $$status

# Generation writes UI and website files used by several checks and must happen first.
prepare:
	$(SERIAL_MAKE) -C cli gen

cli-checks:
	@status=0; \
	run_check() { label=$$1; shift; printf 'START: %s\n' "$$label"; log=$$(mktemp "$${TMPDIR:-/tmp}/boe-check.XXXXXX") || { printf 'FAIL: %s\n' "$$label" >&2; [ -z "$${CHECK_SUMMARY_FILE:-}" ] || printf 'FAIL\t%s\n' "$$label" >>"$$CHECK_SUMMARY_FILE"; status=1; return 1; }; if "$$@" >"$$log" 2>&1; then rm -f "$$log"; printf 'PASS: %s\n' "$$label"; [ -z "$${CHECK_SUMMARY_FILE:-}" ] || printf 'PASS\t%s\n' "$$label" >>"$$CHECK_SUMMARY_FILE"; else cat "$$log" >&2; rm -f "$$log"; printf 'FAIL: %s\n' "$$label" >&2; [ -z "$${CHECK_SUMMARY_FILE:-}" ] || printf 'FAIL\t%s\n' "$$label" >>"$$CHECK_SUMMARY_FILE"; status=1; return 1; fi; }; \
	run_check cli/lint $(SERIAL_MAKE) -C cli lint; \
	run_check cli/build_image $(SERIAL_MAKE) -C cli build_image; \
	run_check cli/test-coverage $(SERIAL_MAKE) -C cli test-coverage; \
	run_check cli/test-e
	2e $(SERIAL_MAKE) -C cli test-e2e; \
	exit $$status

# CLI E2E generation writes the shared UI bundle, so UI checks wait for all CLI checks.
ui-checks:
	@status=0; \
	run_check() { label=$$1; shift; printf 'START: %s\n' "$$label"; log=$$(mktemp "$${TMPDIR:-/tmp}/boe-check.XXXXXX") || { printf 'FAIL: %s\n' "$$label" >&2; [ -z "$${CHECK_SUMMARY_FILE:-}" ] || printf 'FAIL\t%s\n' "$$label" >>"$$CHECK_SUMMARY_FILE"; status=1; return 1; }; if "$$@" >"$$log" 2>&1; then rm -f "$$log"; printf 'PASS: %s\n' "$$label"; [ -z "$${CHECK_SUMMARY_FILE:-}" ] || printf 'PASS\t%s\n' "$$label" >>"$$CHECK_SUMMARY_FILE"; else cat "$$log" >&2; rm -f "$$log"; printf 'FAIL: %s\n' "$$label" >&2; [ -z "$${CHECK_SUMMARY_FILE:-}" ] || printf 'FAIL\t%s\n' "$$label" >>"$$CHECK_SUMMARY_FILE"; status=1; return 1; fi; }; \
	run_check ui/lint $(SERIAL_MAKE) -C ui lint; \
	run_check ui/test-coverage $(SERIAL_MAKE) -C ui test-coverage; \
	exit $$status

website-check:
	@status=0; \
	run_check() { label=$$1; shift; printf 'START: %s\n' "$$label"; log=$$(mktemp "$${TMPDIR:-/tmp}/boe-check.XXXXXX") || { printf 'FAIL: %s\n' "$$label" >&2; [ -z "$${CHECK_SUMMARY_FILE:-}" ] || printf 'FAIL\t%s\n' "$$label" >>"$$CHECK_SUMMARY_FILE"; status=1; return 1; }; if "$$@" >"$$log" 2>&1; then rm -f "$$log"; printf 'PASS: %s\n' "$$label"; [ -z "$${CHECK_SUMMARY_FILE:-}" ] || printf 'PASS\t%s\n' "$$label" >>"$$CHECK_SUMMARY_FILE"; else cat "$$log" >&2; rm -f "$$log"; printf 'FAIL: %s\n' "$$label" >&2; [ -z "$${CHECK_SUMMARY_FILE:-}" ] || printf 'FAIL\t%s\n' "$$label" >>"$$CHECK_SUMMARY_FILE"; status=1; return 1; fi; }; \
	if cd website; then run_check website/npm-ci npm ci; run_check website/build npm run build; else printf 'START: website/cd\n'; printf 'FAIL: website/cd\n' >&2; [ -z "$${CHECK_SUMMARY_FILE:-}" ] || printf 'FAIL\twebsite/cd\n' >>"$$CHECK_SUMMARY_FILE"; status=1; fi; \
	exit $$status

composer-check:
	@status=0; \
	run_check() { label=$$1; shift; printf 'START: %s\n' "$$label"; log=$$(mktemp "$${TMPDIR:-/tmp}/boe-check.XXXXXX") || { printf 'FAIL: %s\n' "$$label" >&2; [ -z "$${CHECK_SUMMARY_FILE:-}" ] || printf 'FAIL\t%s\n' "$$label" >>"$$CHECK_SUMMARY_FILE"; status=1; return 1; }; if "$$@" >"$$log" 2>&1; then rm -f "$$log"; printf 'PASS: %s\n' "$$label"; [ -z "$${CHECK_SUMMARY_FILE:-}" ] || printf 'PASS\t%s\n' "$$label" >>"$$CHECK_SUMMARY_FILE"; else cat "$$log" >&2; rm -f "$$log"; printf 'FAIL: %s\n' "$$label" >&2; [ -z "$${CHECK_SUMMARY_FILE:-}" ] || printf 'FAIL\t%s\n' "$$label" >>"$$CHECK_SUMMARY_FILE"; status=1; return 1; fi; }; \
	run_check composer/format $(SERIAL_MAKE) -C extensions/composer format; \
	run_check composer/lint $(SERIAL_MAKE) -C extensions/composer lint; \
	run_check composer/build $(SERIAL_MAKE) -C extensions/composer build; \
	run_check composer/build_plugins $(SERIAL_MAKE) -C extensions/composer build_plugins; \
	run_check composer/test-coverage $(SERIAL_MAKE) -C extensions/composer test-coverage; \
	run_check composer/test-e2e go -C extensions/composer test -tags e2e -timeout 5m -v; \
	exit $$status

extensions-check: $(EXTENSION_CHECK_TARGETS)

define EXTENSION_CHECK_template
ifneq ($(wildcard extensions/$(1)/Cargo.toml),)
extension-check-$(1):
	@status=0; \
	run_check() { label=$$$$1; shift; printf 'START: %s\n' "$$$$label"; log=$$$$(mktemp "$$$${TMPDIR:-/tmp}/boe-check.XXXXXX") || { printf 'FAIL: %s\n' "$$$$label" >&2; [ -z "$$$${CHECK_SUMMARY_FILE:-}" ] || printf 'FAIL\t%s\n' "$$$$label" >>"$$$$CHECK_SUMMARY_FILE"; status=1; return 1; }; if "$$$$@" >"$$$$log" 2>&1; then rm -f "$$$$log"; printf 'PASS: %s\n' "$$$$label"; [ -z "$$$${CHECK_SUMMARY_FILE:-}" ] || printf 'PASS\t%s\n' "$$$$label" >>"$$$$CHECK_SUMMARY_FILE"; else cat "$$$$log" >&2; rm -f "$$$$log"; printf 'FAIL: %s\n' "$$$$label" >&2; [ -z "$$$${CHECK_SUMMARY_FILE:-}" ] || printf 'FAIL\t%s\n' "$$$$label" >>"$$$$CHECK_SUMMARY_FILE"; status=1; return 1; fi; }; \
	run_check extension/$(1)/format $(SERIAL_MAKE) -C extensions format-rust EXTENSION_PATH=$(1); \
	run_check extension/$(1)/lint $(SERIAL_MAKE) -C extensions lint-rust EXTENSION_PATH=$(1); \
	run_check extension/$(1)/build $(SERIAL_MAKE) -C extensions build-rust EXTENSION_PATH=$(1); \
	run_check extension/$(1)/test-coverage $(SERIAL_MAKE) -C extensions test-rust-coverage EXTENSION_PATH=$(1); \
	exit $$$$status
else ifneq ($(wildcard extensions/$(1)/go.mod),)
extension-check-$(1):
	@status=0; run_check() { label=$$$$1; shift; printf 'START: %s\n' "$$$$label"; log=$$$$(mktemp "$$$${TMPDIR:-/tmp}/boe-check.XXXXXX") || { printf 'FAIL: %s\n' "$$$$label" >&2; [ -z "$$$${CHECK_SUMMARY_FILE:-}" ] || printf 'FAIL\t%s\n' "$$$$label" >>"$$$$CHECK_SUMMARY_FILE"; status=1; return 1; }; if "$$$$@" >"$$$$log" 2>&1; then rm -f "$$$$log"; printf 'PASS: %s\n' "$$$$label"; [ -z "$$$${CHECK_SUMMARY_FILE:-}" ] || printf 'PASS\t%s\n' "$$$$label" >>"$$$$CHECK_SUMMARY_FILE"; else cat "$$$$log" >&2; rm -f "$$$$log"; printf 'FAIL: %s\n' "$$$$label" >&2; [ -z "$$$${CHECK_SUMMARY_FILE:-}" ] || printf 'FAIL\t%s\n' "$$$$label" >>"$$$$CHECK_SUMMARY_FILE"; status=1; return 1; fi; }; \
	printf 'START: extension/$(1)/read-manifest\n'; if type=$$$$(go tool -modfile=tools/go.mod yq '.type' extensions/$(1)/manifest.yaml); then printf 'PASS: extension/$(1)/read-manifest\n'; [ -z "$$$${CHECK_SUMMARY_FILE:-}" ] || printf 'PASS\textension/$(1)/read-manifest\n' >>"$$$$CHECK_SUMMARY_FILE"; else printf 'FAIL: extension/$(1)/read-manifest\n' >&2; [ -z "$$$${CHECK_SUMMARY_FILE:-}" ] || printf 'FAIL\textension/$(1)/read-manifest\n' >>"$$$$CHECK_SUMMARY_FILE"; status=1; fi; \
	case "$$$$type" in \
		wasm) build=build-wasm ;; \
		ext_proc) build=build-extproc ;; \
		*) printf 'START: extension/$(1)/select-build\n'; printf 'Unsupported Go extension type: %s (extensions/$(1))\n' "$$$$type" >&2; printf 'FAIL: extension/$(1)/select-build\n' >&2; [ -z "$$$${CHECK_SUMMARY_FILE:-}" ] || printf 'FAIL\textension/$(1)/select-build\n' >>"$$$$CHECK_SUMMARY_FILE"; status=1; build= ;; \
	esac; \
	run_check extension/$(1)/format $(SERIAL_MAKE) -C extensions format-go EXTENSION_PATH=$(1); \
	run_check extension/$(1)/lint $(SERIAL_MAKE) -C extensions lint-go EXTENSION_PATH=$(1); \
	if [ -n "$$$$build" ]; then run_check extension/$(1)/build $(SERIAL_MAKE) -C extensions "$$$$build" EXTENSION_PATH=$(1); fi; \
	run_check extension/$(1)/test-coverage $(SERIAL_MAKE) -C extensions test-go-coverage EXTENSION_PATH=$(1); \
	exit $$$$status
else
extension-check-$(1):
	@printf 'START: extension/$(1)/e2e-coverage\n'; printf 'PASS: extension/$(1)/e2e-coverage (covered by E2E tests)\n'; [ -z "$$$${CHECK_SUMMARY_FILE:-}" ] || printf 'PASS\textension/$(1)/e2e-coverage\n' >>"$$$$CHECK_SUMMARY_FILE"
endif
endef
$(foreach ext,$(EXTENSION_NAMES),$(eval $(call EXTENSION_CHECK_template,$(ext))))

extensions-e2e-check:
	@status=0; \
	run_check() { label=$$1; shift; printf 'START: %s\n' "$$label"; log=$$(mktemp "$${TMPDIR:-/tmp}/boe-check.XXXXXX") || { printf 'FAIL: %s\n' "$$label" >&2; [ -z "$${CHECK_SUMMARY_FILE:-}" ] || printf 'FAIL\t%s\n' "$$label" >>"$$CHECK_SUMMARY_FILE"; status=1; return 1; }; if "$$@" >"$$log" 2>&1; then rm -f "$$log"; printf 'PASS: %s\n' "$$label"; [ -z "$${CHECK_SUMMARY_FILE:-}" ] || printf 'PASS\t%s\n' "$$label" >>"$$CHECK_SUMMARY_FILE"; else cat "$$log" >&2; rm -f "$$log"; printf 'FAIL: %s\n' "$$label" >&2; [ -z "$${CHECK_SUMMARY_FILE:-}" ] || printf 'FAIL\t%s\n' "$$label" >>"$$CHECK_SUMMARY_FILE"; status=1; return 1; fi; }; \
	run_check extensions-e2e/check $(SERIAL_MAKE) -C extensions/tests/e2e check; \
	exit $$status

extensions-e2e-tests:
	# The suite uses a fixed FTW port and shared log paths; keep matrix entries serial.
	@status=0; run_check() { label=$$1; shift; printf 'START: %s\n' "$$label"; log=$$(mktemp "$${TMPDIR:-/tmp}/boe-check.XXXXXX") || { printf 'FAIL: %s\n' "$$label" >&2; [ -z "$${CHECK_SUMMARY_FILE:-}" ] || printf 'FAIL\t%s\n' "$$label" >>"$$CHECK_SUMMARY_FILE"; status=1; return 1; }; if "$$@" >"$$log" 2>&1; then rm -f "$$log"; printf 'PASS: %s\n' "$$label"; [ -z "$${CHECK_SUMMARY_FILE:-}" ] || printf 'PASS\t%s\n' "$$label" >>"$$CHECK_SUMMARY_FILE"; else cat "$$log" >&2; rm -f "$$log"; printf 'FAIL: %s\n' "$$label" >&2; [ -z "$${CHECK_SUMMARY_FILE:-}" ] || printf 'FAIL\t%s\n' "$$label" >>"$$CHECK_SUMMARY_FILE"; status=1; return 1; fi; }; \
	for envoy_version in dev 1.38.0 1.39.0; do \
		run_check extensions-e2e/$$envoy_version $(SERIAL_MAKE) -C extensions/tests/e2e test ENVOY_VERSION=$$envoy_version; \
	done; exit $$status
