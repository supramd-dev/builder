.PHONY: dev-frontend dev-backend build serve test smoke-test seed-demo clean adduser

# --- Frontend (Vite dev server, hot reload, :5173) ---
dev-frontend:
	cd frontend && npm run dev

# --- Backend (Go, :8080) ---
dev-backend:
	cd server && go run .

build:
	cd frontend && npm run build

serve: build
	cd server && go run .

test:
	cd server && go test ./...

# End-to-end API smoke test against a running server (default :8080).
# Override with USER/EMAIL/PASSWORD if needed.
smoke-test:
	scripts/api-smoke.sh

# Populate a running server with demo dashboard data (pushes + runs).
seed-demo:
	scripts/seed-demo.sh

clean:
	rm -rf frontend/dist frontend/node_modules server/md-builder.db md-builder.db

# Create a user via the CLI. Override: make adduser USER=alice EMAIL=a@b.c [PASSWORD=x]
adduser:
	cd server && go run . adduser -username $(USER) -email $(EMAIL)
