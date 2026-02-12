.PHONY: build run clean test help

# Build the deployment engine
build:
	@echo "Building deployment engine..."
	@go build -o bin/deployment_engine ./cmd/serve

# Run the deployment engine locally
run: build
	@echo "Running deployment engine..."
	@./bin/deployment_engine

# Clean build artifacts
clean:
	@echo "Cleaning build artifacts..."
	@rm -rf bin/

# Run tests
test:
	@echo "Running tests..."
	@go test -v ./...

# Install dependencies
deps:
	@echo "Installing dependencies..."
	@go mod download
	@go mod tidy

# Display help
help:
	@echo "Deployment Engine Makefile"
	@echo ""
	@echo "Available targets:"
	@echo "  build  - Build the deployment engine binary"
	@echo "  run    - Build and run the deployment engine"
	@echo "  clean  - Remove build artifacts"
	@echo "  test   - Run test suite"
	@echo "  deps   - Download and tidy dependencies"
	@echo "  help   - Show this help message"
