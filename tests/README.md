# Tests

Test suites for Bureau, organized by scope and purpose.

## Structure

### `unit/`

Unit tests that:

- Test individual functions and methods in isolation
- Use mocks for external dependencies
- Run fast (milliseconds)
- Can run without network or external services

Convention: Unit tests live alongside code (`*_test.go`, `test_*.py`) but shared fixtures and utilities live here.

### `integration/`

Integration tests that:

- Test multiple components together
- May use real Matrix connections (to test server)
- May use real databases
- Run slower (seconds to minutes)

### `qa/`

Agent-based QA scenarios:

- End-to-end workflow tests
- Defined as scenarios agents can execute
- Validate that the system behaves correctly from a user perspective

## Running Tests

```bash
# All tests
bazel test //...

# Unit tests only
bazel test //... --test_tag_filters=unit

# Integration tests (requires setup)
bazel test //... --test_tag_filters=integration

# With coverage
bazel coverage //...
```

## Test Data

Test fixtures and golden files live in `testdata/` subdirectories alongside the tests that use them.

## Conventions

- Tests are not optional; new code needs tests
- Prefer table-driven tests for multiple cases
- Use descriptive test names that explain the scenario
- Mock external services; don't depend on network in unit tests
