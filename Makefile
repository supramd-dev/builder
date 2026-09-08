.PHONY: dev-frontend dev-backend build serve test clean adduser

# --- Frontend (Vite dev server, hot reload, :5173) ---
dev-frontend:
	cd frontend && npm run dev

# --- Backend (Go, :8080) ---
dev-backend:
	go run ./server

build:
	cd frontend && npm run build

serve: build
	go run ./server

test:
	cd server && go test ./...

clean:
	rm -rf frontend/dist frontend/node_modules server/md-builder.db md-builder.db

# Create a user via the CLI. Override: make adduser USER=alice EMAIL=a@b.c
adduser:
	go run ./server adduser -username $(USER) -email $(EMAIL)
