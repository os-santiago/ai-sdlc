# Normalized Event Schema Versioning

## Overview
The normalized event schema follows semantic versioning (MAJOR.MINOR.PATCH) to ensure forge adapters can evolve independently while maintaining compatibility guarantees.

## Versioning Rules

### MAJOR Version (Breaking Changes)
Increment when making **backward-incompatible** changes to the schema, such as:
- Removing or renaming required fields
- Changing field types in incompatible ways (e.g., string → integer)
- Removing enum values
- Changing the semantic meaning of existing fields
- Adding new required fields without defaults

**Impact:** Adapters must be updated to produce the new version. Consumers must upgrade to understand the new schema.

### MINOR Version (Backward-Compatible Changes)
Increment when adding **backward-compatible** functionality, such as:
- Adding new optional fields
- Adding new enum values
- Adding new allowed values to string fields (when validation permits)
- Adding new object properties that are not required

**Impact:** Existing v1.x consumers continue to work with v1.y (y > x) schemas. Adapters can emit the new version without breaking existing consumers.

### PATCH Version (Editorial Changes)
Increment for **non-functional** changes, such as:
- Fixing spelling/grammar in descriptions
- Adding clarifying examples
- Correcting schema formatting
- Adding annotations that don't affect validation

**Impact:** No changes needed for adapters or consumers.

## Version Identification
Events MUST include a `version` field conforming to the schema:
```json
{
  "version": "1.0.0",
  "entity": { ... },
  "transition": "...",
  "actor": { ... }
}
```

## Compatibility Guidelines

### For Adapter Authors
1. **Target a specific version** - Emit events that conform exactly to one schema version
2. **Version detection** - Consumers should check the `version` field to determine how to parse the event
3. **Fallback strategy** - Consider supporting multiple versions during transition periods
4. **Testing** - Validate emitted events against the target schema using JSON schema validators

### For Consumer Authors
1. **Version checking** - Always read the `version` field before parsing
2. **Version ranges** - Specify which schema versions your consumer supports
3. **Graceful degradation** - Handle unknown versions appropriately (reject or fallback)
4. **Validation** - Use JSON schema validation to ensure conformance

## Migration Example: v1.0.0 → v1.1.0
**Change:** Added optional `repository.url` field
- **Adapter change:** Begin populating the new field when available
- **Consumer change:** Can safely ignore the field if not needed, or use it when present
- **Compatibility:** v1.0.0 consumers work unchanged with v1.1.0 events

## Migration Example: v1.0.0 → v2.0.0
**Change:** Renamed `entity.state` to `entity.lifecycle_state` and made it required
- **Adapter change:** Map forge state to new field name, ensure it's always present
- **Consumer change:** Update code to use new field name
- **Compatibility:** v1.0.0 consumers will fail with v2.0.0 events (missing required field)

## Current Schema Location
The canonical schema is maintained at:
`spec/normalized-event.schema.json`

## Changelog

### v1.0.0 (Initial)
- Entity kinds: work_item, change_request, conversation
- Basic entity fields: id, number, title, description, state, labels, timestamps
- Transition: forge-neutral actions
- Actor: id, type, login, display_name, avatar_url
- Repository: id, name, url (optional)

## Forge Adapter Implementation
Adapters SHOULD:
1. Map forge-specific webhook payloads to this normalized format
2. Preserve all available information without loss
3. Use null for unavailable fields rather than omitting them
4. Validate output against the target schema before emission
5. Document any forge-specific limitations or mappings

Consumers SHOULD:
1. Validate incoming events against expected schema versions
2. Treat the normalized event as the single source of truth
3. Avoid accessing forge-specific fields directly from raw payloads