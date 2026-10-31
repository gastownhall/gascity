# internal/schemacontract — change guide

`internal/schemacontract` gates the shape of every built-in CLI JSON schema
(`schemas/**/*.schema.json`) against the committed `baseline.json`.
`TestSchemaContractBaseline` fails CI when a schema changes incompatibly
without a `schema_version` bump. Design and policy:
`engdocs/design/json-cli-support.md` ("Schema Baseline Gate").

## Changing a schema under `schemas/`

1. Edit the schema.
2. Run `bazel run //cmd/gen-schema-baseline` from the repository root and
   commit the resulting `baseline.json`. The generator only seeds, adds a
   version entry, or appends records; it never edits or deletes one.
3. A breaking change (field removed or renamed, type widened, `const`
   changed or dropped, `required` relaxed, variant removed, framing changed)
   also bumps the root `properties.schema_version.const`. Fill in the new
   entry's `note` (written as a release note) and its `issue` or `pr`, and
   mark the commit `!` or with a `BREAKING CHANGE:` footer.
4. Removing a schema is breaking: delete its baseline record deliberately.

Enums are not recorded, only `const`. A meaning change with an unchanged
shape is declared by the author with a bump; the gate does not detect it.

## Invariants

- The package is stdlib-only and does no I/O beyond the `fs.FS` it is given.
- `baseline.json` lives outside `schemas/` because `schemas_embed.go`
  embeds `schemas/**`. Do not hand-edit records; regenerate.
- The extractor fails on unknown schema keywords instead of ignoring them.
  A new keyword is either interpreted or added to the documented ignore set
  in `extract.go`, with a test.
