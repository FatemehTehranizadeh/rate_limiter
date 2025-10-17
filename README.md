# Rate Limiter (Go)

A concurrency-safe and high-performance rate-limiting library written in pure Go.  
It implements multiple algorithms like Token Bucket, Leaky Bucket, and Sliding Window.

## 🚀 Project Goals
- Practice advanced concurrency (sync.Mutex, sync.Cond, sync.Pool, etc.)
- Learn struct design and interface patterns
- Prepare for REST API and microservices development

## 🧩 Folder Structure
rate-limiter/
├─ cmd/demo/ # Demo program to run examples
├─ internal/limiter/ # Core limiter algorithms
├─ internal/store/ # Storage interface and in-memory implementation
├─ internal/manager/ # Manages limiters per key/config
├─ internal/metrics/ # Collects and reports statistics
├─ pkg/ratelimit/ # Public API
├─ test/ # Integration tests
├─ bench/ # Benchmarks
└─ README.md


## 🧰 Commands
```bash
make test     # run tests
make lint     # run static analysis
make run      # run demo app
```


## 🧠 Concepts Covered

- Concurrency and synchronization

- Advanced Go patterns

- Clean architecture and separation of concerns


---

### 🧱 Verify
Run each:
```bash
go mod tidy
make fmt
make lint
make test
```
