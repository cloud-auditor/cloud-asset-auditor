package policy

// StarterRules is the commented example ruleset printed by
// `auditor check --init`. It must always Load() cleanly — enforced by a test.
const StarterRules = `# Policy rules for "auditor check".
#
# Each rule selects assets with "match" (glob lists; * is the only wildcard,
# matching is case-insensitive; values within a field OR together, fields AND
# together) and then checks the conditions under "assert". A rule without an
# assert block flags every matched asset — use that to forbid whole classes
# of assets. Severity is one of info|warning|error|critical (default warning).
rules:
  # Every asset that carries tags at all should say who owns it.
  - name: require-owner-tag
    description: Assets must carry an owner tag
    severity: warning
    assert:
      required_tags: [owner]

  # Compute that exists should be running; anything stopped is either waste
  # or drift.
  - name: instances-should-run
    description: Compute instances should be in a running state
    severity: info
    match:
      types: ["oci.instance", "gcp.*instance*"]
    assert:
      status_in: ["running", "active"]

  # Example of a forbid rule: no assert block, so every match is a finding.
  # - name: no-legacy-page-rules
  #   description: Cloudflare Page Rules are deprecated; migrate to Rulesets
  #   severity: error
  #   match:
  #     types: ["cloudflare.page_rule"]

  # Example of value-constrained tags (patterns take a scalar or a list).
  # - name: env-tag-is-canonical
  #   description: env must be one of prod/staging/dev
  #   severity: error
  #   assert:
  #     tag_matches:
  #       env: [prod, staging, dev]
`
