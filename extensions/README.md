# Extensions Directory

This directory will contain community-contributed Envoy extensions.

## Structure

Each extension should be in its own directory with:

```
extension-name/
├── README.md            # Description and usage
├── manifest.yaml        # Metadata (name, version, author, etc.)
├── {go,lua,rs}          # Extension code (language-specific)
└── config.schema.json   # (Optional) JSON schema describing the extension configuration
```

## Extension e2e tests

Extension e2e tests live in the [tests/e2e](./tests/e2e) directory. Take a look at the README file
there for details on writing end-to-end tests for your extension.

## Executable manifest examples

Use `config` and ordered `commands[].argv` in a manifest example to generate its `code` from a
real BOE/Envoy run. Leave out `code` on the first run; `config: {}` is valid when no settings
are needed. Existing hand-written, code-only examples remain supported.

The [Executable Extension Examples guide](../website/src/pages/docs/writing-extension-examples.mdx)
walks through authoring, generation, checking, and review. It covers:

- `preStart` commands and checked-in `examples/` fixtures for startup inputs.
- Command comments, expected exit codes, and bounded HTTP-status retries.
- `volatileHeaders` and scoped date/duration comparison rules.
- `elideOutput: true` and standalone `...` lines for compact, checked output excerpts.
- Supported placeholders, execution options, and current runner limits.

Run the Makefile entrypoints from the repository root, with a path relative to `extensions/`:

```sh
make -C extensions update-examples EXTENSION_PATH=composer/opa
make -C extensions check-examples EXTENSION_PATH=composer/opa
```

Review the generated manifest diff before committing. `update-examples` writes only after all
selected examples execute successfully; `check-examples` compares without writing. The normal
`check` target also includes `check-examples`. Select `EXTENSION_PATH=composer` to run the bundle's
root and child manifests, or a different extension directory to check that extension.

For elision, generate the full transcript first, set `elideOutput: true`, and replace incidental
whole lines inside fenced `text` output with standalone `...` lines. Keep HTTP status lines and
header terminators visible. Run `update-examples` again to refresh the excerpt's metadata, then
`check-examples`. Retained lines still follow volatility and format rules. A material change to a
retained line expands that output block on update so it can be reviewed.

Pass options such as `-timeout 5m`, `-envoy-version 1.38.0`, `-envoy-path /path/to/envoy`, or
`-boe /path/to/boe` through `EXAMPLES_ARGS`. The generator's
[requirements and invariants](../cli/tools/gen-examples/README.md) define its detailed guarantees.

## Contributing an Extension

1. Fork this repository
2. Create a new directory under `extensions/` with your extension name
3. Add all required files (see structure above)
4. Ensure your README includes:
   - Description of what the extension does
   - Installation/usage instructions
   - Configuration options
   - Examples
5. Open a pull request

## Extension Guidelines

- Extensions should be focused on a single responsibility
- Include comprehensive documentation
- Provide working examples
- Follow Envoy best practices
- Include tests where applicable
