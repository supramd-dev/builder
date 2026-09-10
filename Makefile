.PHONY: dev-frontend dev-backend build serve test smoke-test seed-demo clean adduser seed

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

# Seed the database with demo data: a demo user (demo / demo-pass-123),
# three fake environments, five pushes, regression/unit/build runs and two
# finished task graphs with logs. Pass FORCE=1 to rebuild the graphs.
seed-demo:
	scripts/seed-demo.sh $(if $(FORCE),--force,)

clean:
	rm -rf frontend/dist frontend/node_modules server/md-builder.db md-builder.db

# Create a user via the CLI. Override: make adduser USER=alice EMAIL=a@b.c [PASSWORD=x]
adduser:
	cd server && go run . adduser -username $(USER) -email $(EMAIL)

# Seed demo data directly via the CLI (same as seed-demo, without rebuild).
# Override with DSN=... if needed.
seed:
	cd server && go run . seed $(if $(FORCE),-force,)
