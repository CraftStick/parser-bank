.PHONY: build bot record map report doctor driver test fmt vet tidy clean

build:
	go build -o bin/bot ./cmd/bot
	go build -o bin/recorder ./cmd/recorder
	go build -o bin/mapper ./cmd/mapper
	go build -o bin/doctor ./cmd/doctor

# Проверить окружение, не заходя в банк
doctor:
	go run ./cmd/doctor

# Поставить драйвер Playwright в обход выключенного CDN Microsoft
driver:
	./scripts/install-driver.sh

# Основной процесс.
#
# Запускаем собранный бинарник, а не `go run`: при Ctrl+C сигнал приходит всей
# группе процессов, и `go run` умирает от него сам, из-за чего make сообщает об
# ошибке, хотя бот остановился штатно. Через exec процессом-потомком make
# становится сам бот, а он выходит с нулевым кодом.
bot:
	@go build -o bin/bot ./cmd/bot
	@exec ./bin/bot

# Разовый discovery: снять ручки банка и составить endpoints.json
record:
	go run ./cmd/recorder

# Пересобрать endpoints.json по уже снятым дампам, не заходя в банк
map:
	go run ./cmd/mapper

# То же, но только показать отчёт
report:
	go run ./cmd/mapper -report

test:
	go test ./...

fmt:
	gofmt -w .

vet:
	go vet ./...

tidy:
	go mod tidy

# Сборка под VPS (Linux x86_64). Драйвер чисто на Go, так что cgo не нужен.
build-linux:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o bin/bot-linux ./cmd/bot
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o bin/recorder-linux ./cmd/recorder
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o bin/mapper-linux ./cmd/mapper
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o bin/doctor-linux ./cmd/doctor

clean:
	rm -rf bin
