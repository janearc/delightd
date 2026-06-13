#!/usr/bin/env bash
set -e

# We only check coverage on internal packages since main.go is a blocking daemon loop
echo "Running tests and gathering coverage for pkg/ and config/..."
go test -coverprofile=coverage.out ./pkg/... ./config/... > /dev/null

coverage=$(go tool cover -func=coverage.out | tail -n 1 | awk '{print $3}' | sed 's/%//')
passed=$(awk -v cov="$coverage" 'BEGIN {print (cov >= 80.0) ? "1" : "0"}')

if [ "$passed" -eq "0" ]; then
    echo "❌ ERROR: Test coverage $coverage% is below the 80% floor required by AGENTS.md!"
    echo "Please add missing tests before pushing."
    go tool cover -func=coverage.out | grep -v "100.0%"
    exit 1
fi

echo "✅ SUCCESS: Test coverage is $coverage% (Meets 80% hyperscaler standard)"
exit 0
