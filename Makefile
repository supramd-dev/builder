.PHONY: dev-frontend dev-backend build serve clean

# --- Frontend ---
dev-frontend:
	cd frontend && npm run dev

# --- Backend ---
dev-backend:
	go run ./server

build:
	cd frontend && npm run build

serve: build
	go run ./server

clean:
	rm -rf frontend/dist frontend/node_modules
