# Независимое техническое ревью

Дата: 2026-11-06.

Проверялись `TASK.md`, `README.md`, все исходники, миграции, unit- и integration-тесты, Dockerfile и Compose-конфигурация. Реализация не изменялась. Дополнительные проверки выполнялись против одноразовых PostgreSQL и контейнеров приложения; временные файлы размещались в `/tmp/opencode/kimi-booking-review`.

## Краткое заключение

**Решение в целом соответствует основным требованиям `TASK.md`.** Сохранность данных, запрет перепродажи, идемпотентное создание, повторная и конкурентная отмена, работа двух экземпляров с общей PostgreSQL, подготовка схемы и повторный запуск подтверждены кодом и проверками на настоящей PostgreSQL 16. Нарушений этих инвариантов не найдено.

Подтверждены один дефект **P2** и два дефекта **P3**; дефектов P0/P1 не обнаружено:

- **P2** — документированные лимиты длины в символах фактически проверяются в байтах, поэтому отклоняются допустимые Unicode-строки.
- **P3** — недоверенный URL может вставлять перевод строки в журнал и подделывать его записи.
- **P3** — ответы ServeMux на неизвестный маршрут и неподдерживаемый метод не соответствуют заявленному JSON-контракту ошибок.

Эти замечания не отменяют подтверждённые гарантии целостности бронирований.

## Подтверждённые дефекты

### P2 — Лимиты длины имени и ключа идемпотентности считаются в байтах, а не в символах

**Файлы и строки:**

- `internal/service/service.go:67-68` — проверка имени через `len(name)`.
- `internal/service/service.go:94-96` — проверка ключа через `len(clientToken)`.
- Документированный контракт: `README.md:61-62`, `README.md:90-92`.

**Условие проявления:** клиент передаёт строку из ASCII вне диапазона либо обычную Unicode-строку, длина которой не превышает заявленные 200 символов для имени или 128 символов для ключа, но размер UTF-8 больше соответствующего числа байт.

**Последствия:** корректный по документации запрос получает `400 invalid_request`. Для русского текста ограничение фактически наступает примерно вдвое раньше. Это особенно заметно для API с русскоязычной документацией и примерами.

**Доказательство:** против отдельного тестового контейнера выполнены запросы со строками ровно на документированной границе:

```python
import json
import urllib.error
import urllib.request


def post(path, body):
    request = urllib.request.Request(
        "http://localhost:8080" + path,
        data=json.dumps(body).encode(),
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    try:
        with urllib.request.urlopen(request) as response:
            return response.status, json.load(response)
    except urllib.error.HTTPError as error:
        return error.code, json.load(error)


print(post("/events", {"name": "я" * 200, "capacity": 1}))
status, event = post("/events", {"name": "x" * 200, "capacity": 1})
print(status, event["id"])
print(post("/bookings", {
    "event_id": event["id"],
    "seats": [1],
    "idempotency_key": "й" * 128,
}))
print(post("/bookings", {
    "event_id": event["id"],
    "seats": [1],
    "idempotency_key": "k" * 128,
}))
```

Фактический результат:

```text
200 Cyrillic characters in name: 400 name must be at most 200 characters
200 ASCII characters in name: 201
128 Cyrillic characters in idempotency key: 400 idempotency key must be at most 128 characters
128 ASCII characters in idempotency key: 201
```

**Направление исправления:** выбрать и единообразно зафиксировать семантику лимита. Если документация сохраняет символы, проверять количество Unicode code points (`utf8.RuneCountInString`) и добавить граничные тесты для ASCII и мультибайтных строк. Альтернатива — явно документировать лимит в байтах UTF-8; тогда текущее поведение следует считать осознанным контрактом.

### P3 — Перевод строки в URL позволяет подделывать записи журнала

**Файл и строки:** `internal/httpapi/server.go:289-295`, особенно логирование `r.URL.Path` в строке 294.

**Условие проявления:** недоверенный HTTP-клиент запрашивает путь с percent-encoded управляющим символом, например `%0A`. `net/http` декодирует его в `r.URL.Path`, после чего middleware печатает путь без экранирования.

**Последствия:** одна HTTP-операция создаёт несколько строк журнала; содержимое следующей строки контролируется клиентом. Это затрудняет разбор журнала и позволяет имитировать записи, что может вводить в заблуждение при ручном расследовании или обработке строчных логов.

**Доказательство и воспроизведение на собственном тестовом Compose-проекте:**

```bash
docker compose -p kimi-review-booking up --build -d
curl -sS --path-as-is \
  'http://localhost:8080/review%0AFORGED%20status=200'
docker logs kimi-review-booking-app-1 2>&1 | grep -n -B 1 -A 1 FORGED
```

Фактический фрагмент журнала:

```text
170-2026/11/06 10:59:58 GET /review
171:FORGED status=200 404 23µs
```

**Направление исправления:** логировать экранированное представление пути (например, `%q`) либо `EscapedPath()` с отдельной санитизацией управляющих символов. Добавить middleware-тест, проверяющий, что один запрос не создаёт дополнительных физических строк журнала.

### P3 — Неизвестные маршруты и методы возвращают text/plain вместо заявленного JSON

**Файлы и строки:**

- `internal/httpapi/server.go:20-37` — зарегистрированы только конкретные маршруты; ответы 404/405 самого `http.ServeMux` не переопределены.
- `README.md:46-47` — заявлено, что все ответы JSON, а ошибки имеют объект `error`.

**Условие проявления:** запрос на незарегистрированный путь или запрос с неподдерживаемым методом к существующему ресурсу.

**Последствия:** клиент, который по документации всегда декодирует ошибку как JSON, получает ошибку разбора вместо структурированного `code`/`message`. Основные сценарии бронирования не затрагиваются.

**Доказательство:**

```bash
curl -i http://localhost:8080/does-not-exist
curl -i -X DELETE http://localhost:8080/events
```

Фактически получены:

```text
404 text/plain; charset=utf-8
404 page not found

405 text/plain; charset=utf-8
Method Not Allowed
```

**Направление исправления:** либо маршрутизировать 404 и 405 через общий JSON-обработчик ошибок и покрыть их тестами, либо сузить утверждение README до ответов реализованных API-операций.

## Архитектура

### Соразмерность и разделение ответственности

Структура из нескольких небольших пакетов соразмерна задаче; избыточных слоёв, фреймворков и инфраструктурных зависимостей нет:

- `cmd/server/main.go` ограничивается конфигурацией, сборкой зависимостей, запуском и graceful shutdown.
- `internal/httpapi/server.go` отвечает за HTTP-контракт, строгий разбор JSON, mapping доменных ошибок и middleware.
- `internal/service/service.go` содержит входные ограничения и нормализацию мест.
- `internal/store/postgres.go` владеет SQL и транзакционными границами; `migrations.go` отдельно содержит схему и её применение.
- `internal/domain/domain.go` разделяет типы и типизированные ошибки между транспортом, сервисом и хранилищем.

Направление зависимостей понятно: композиция находится в `main`, HTTP зависит от сервиса и домена, сервис — от узкого `Store` и домена, PostgreSQL-реализация — от домена. Циклических зависимостей и утечки SQL в HTTP нет. То, что сервис делегирует read-only операции почти без собственной логики, не является лишним слоем: он даёт единую точку валидации и позволяет изолированно тестировать правила.

### Бизнес-инварианты и транзакции

Размещение критических инвариантов в PostgreSQL — сильное и уместное решение:

- частичный уникальный индекс в `internal/store/migrations.go:43-47` запрещает два активных владельца одного места независимо от числа процессов;
- уникальность `(event_id, client_token)` в `migrations.go:25-33` закрепляет идемпотентность на уровне данных;
- создание бронирования и всех мест выполняется в одной транзакции (`postgres.go:164-236`), поэтому конфликт откатывает и строку бронирования, и частично вставленные места;
- изменение статуса и освобождение мест атомарны (`postgres.go:243-275`), а блокировка строки сериализует одновременные отмены;
- согласованный snapshot счётчика и списка свободных мест обеспечивается read-only repeatable-read транзакцией (`postgres.go:121-161`).

Предварительный `takenSeats` перед вставкой дублирует часть работы уникального индекса, но это оправданный компромисс: индекс остаётся арбитром при гонке, а pre-check даёт точный список занятых мест в обычном конфликте. Цена — один дополнительный запрос на создание.

Вне транзакции после commit выполняется повторное чтение созданного/отменённого бронирования. Поэтому ответ может отражать уже успевшую конкурентную отмену; это согласуется с последовательностью «create → cancel» и не нарушает инварианты, но является осознанной семантикой eventual наблюдения ответа, а не snapshot на момент commit.

### Тестируемость, понятность и стоимость изменений

- `service.Store` позволяет тестировать валидацию без БД; integration-тесты проходят через полный HTTP stack и настоящую PostgreSQL.
- Типизированные доменные ошибки централизованно отображаются в HTTP, поэтому добавление нового клиентского error case не требует разбрасывать SQL-коды по handlers.
- SQL-запросы и комментарии расположены рядом с транзакционной логикой; основные нетривиальные решения объяснены в коде и README.
- Добавление обычных полей или endpoints имеет низкую стоимость. Изменение инвариантов вместимости потребует согласованной правки схемы, транзакций и тестов — это необходимая, а не искусственная связанность.

Компромисс схемы: `booking_seats.event_id` денормализован относительно `bookings.event_id`, чтобы частичный индекс непосредственно защищал `(event_id, seat_no)`. Текущий store всегда заполняет оба значения согласованно, но составного FK, запрещающего рассинхронизацию при появлении другого писателя, нет. Для текущего небольшого сервиса это не подтверждённый дефект; при расширении числа способов записи ограничение целостности стоит пересмотреть.

Самодельный механизм миграций с одной версией, advisory lock и транзакционным DDL пропорционален текущей задаче. При росте числа версий потребуется поддерживать строгую дисциплину применения; нет необходимости заранее вводить внешний мигратор только ради архитектурной формальности.

## Безопасность

### Модель угроз и границы доверия

Рассматривалась локальная/тестовая модель Compose:

- HTTP-клиент недоверен: может отправлять произвольные методы, пути, заголовки, JSON и конкурентные запросы.
- Границы доверия проходят между HTTP и JSON/service-слоем, а также между service и PostgreSQL.
- Хост, Docker daemon, переменные окружения и сама PostgreSQL считаются доверенными; компрометация этих компонентов не входила в модель.
- PostgreSQL доступна приложению по внутренней Compose-сети. Активные проверки выполнялись только против созданных для ревью контейнеров, без внешних целей и тяжёлых нагрузок.

### Результаты проверки

- **SQL-инъекции не обнаружены.** Все пользовательские значения передаются параметрами `$1…$n`; динамические SQL-фрагменты в `takenSeats`/`insertSeats` состоят только из генерируемых placeholders. Имя `x'); DROP TABLE events;--` было сохранено как обычный текст (`201`), последующий `GET /events` вернул `200`, таблица осталась доступна.
- **Недоверенный JSON:** включены `DisallowUnknownFields`, проверка единственного JSON-значения и `MaxBytesReader` на 1 МиБ (`server.go:236-247`). Тело больше 1 МиБ получило понятный `400 invalid_request`, а не записало данные. Малformed JSON и неизвестные поля покрыты штатными integration-тестами.
- **Управляющие символы в пути:** подтверждена отдельная P3-проблема многострочной подделки журнала, описанная выше.
- **Внутренние ошибки:** внешний ответ по умолчанию содержит только `internal server error`; SQL/DSN клиенту не возвращаются (`server.go:258-274`). Подробные ошибки остаются в серверном журнале. Выполненный поиск `DATABASE_URL` и `postgres://booking` в журнале приложения совпадений не дал.
- **Таймауты:** у HTTP-сервера заданы read-header/read/write/idle timeouts (`main.go:52-59`), у пула ограничено число соединений (`postgres.go:41-43`). Отдельных deadlines на SQL-операции нет; устойчивость при зависшей БД отнесена к неподтверждённым рискам.
- **Контейнеры и порты:** приложение запускается от пользователя `app` (UID 10001), а не root. PostgreSQL не публикует host-порт; приложение опубликовано на `0.0.0.0:8080` (и дополнительный тестовый экземпляр — на `8081`).
- **Секреты:** учётные данные `booking/booking` находятся в Compose и DSN. Для локального задания это работоспособная конфигурация, но не секретное хранилище для публичного окружения.
- **Зависимости Go:** `govulncheck` v1.9.0 в source mode завершился с `No vulnerabilities found.` Результат относится к reachable-уязвимостям в проверенных Go-пакетах, stdlib и зависимостях на момент запуска. Он не является сканированием Alpine/PostgreSQL images и не доказывает отсутствие всех уязвимостей. Более старый `govulncheck` v1.1.4 аварийно завершился внутри `x/tools/go/ssa` на Go 1.27.3; это ограничение инструмента, а не дефект приложения.

### Условия публичного развёртывания (не дефекты TASK.md)

`TASK.md` прямо разрешает отсутствие авторизации, поэтому она не оценивается как несоответствие. Однако текущая конфигурация предназначена для доверенного локального/внутреннего запуска:

- `compose.yaml:19-22` использует `sslmode=disable` и публикует HTTP на всех интерфейсах хоста;
- TLS нет, а любой достигший порта клиент может создавать события и отменять бронирования по id;
- статические учётные данные БД не должны переиспользоваться как производственный секрет.

Для публичной эксплуатации необходимы внешняя аутентификация/авторизация, TLS, ограничение сетевой доступности, управление секретами и отдельная оценка rate limiting. Само по себе отсутствие найденных проблем не является доказательством безопасности.

## Неподтверждённые риски

1. **Исчерпание ресурсов при недружественном трафике или degraded PostgreSQL.** Rate limiting отсутствует и прямо указан в ограничениях README; `GET /events` не пагинирован, создание событий не ограничено квотой, `GET /events/{id}` может формировать до 100 000 номеров. Кроме того, нет отдельного statement/request timeout для SQL. При публичном доступе или зависшей БД возможны рост ответов, ожидание пула и накопление goroutines. Нагрузочного или fault-injection теста не проводилось, поэтому фактическая устойчивость и порог отказа не подтверждены.
2. **Совместимость с заранее существующей неподходящей схемой.** `CREATE ... IF NOT EXISTS` безопасно повторяется на схеме, созданной этим приложением, но не исправляет вручную созданные таблицы с теми же именами и другими определениями. Такой источник схемы в условиях задачи не задан; проверка не выполнялась, дефектом текущего сценария это не считается.
3. **Целостность при обходе единственного store.** Как отмечено в архитектуре, согласованность денормализованного `booking_seats.event_id` обеспечивается кодом записи, а не отдельным составным FK. Ошибка проявится только при новом некорректном писателе или будущем изменении SQL; текущие сценарии её не показали.

## Необязательные рекомендации

- Добавить в автоматические тесты найденные границы Unicode, multiline path logging и JSON 404/405; сейчас штатные тесты эти случаи не проверяют.
- Сохранить отдельный smoke/e2e тест с двумя реальными процессами или контейнерами. Текущий `twoStacks` создаёт два независимых pool и HTTP server в одном тестовом процессе; для проверки DB-инвариантов этого достаточно, а реальный двухконтейнерный сценарий в данном ревью выполнен вручную.
- В `TestConcurrentNoOversell` дополнительно сверять активные места каждого успешного ответа со статусом его бронирования и логировать seed/наборы мест при ошибке. Это упростит воспроизведение редких отказов, хотя текущие инвариантные проверки полезны и прошли.
- Для более строгой воспроизводимости supply chain можно закрепить базовые images digest и периодически сканировать ОС-пакеты контейнеров. Текущие major/minor tags успешно собрались, а Go-зависимости уже зафиксированы.

## Фактически выполненные проверки

| Команда / сценарий | Результат |
|---|---|
| `go version && go env GOTOOLCHAIN && go test -count=1 ./...` | Go 1.27.3, toolchain `auto`; unit-тесты прошли. Без `BOOKING_TEST_DATABASE_URL` integration-тесты штатно пропущены. |
| `go vet ./...` | Успешно, замечаний нет. |
| `go mod verify` | `all modules verified`. |
| `docker compose config --quiet` | Конфигурация валидна. |
| `BOOKING_TEST_PG_PORT=56432 scripts/integration.sh` | Одноразовый PostgreSQL 16 поднят; `go test -race -count=1 -timeout 10m ./...` прошёл: `booking/integration 2.891s`, `booking/internal/service 1.009s`. Контейнер удалён trap-обработчиком. |
| `go run golang.org/x/vuln/cmd/govulncheck@v1.1.4 ./...` | Не завершено из-за panic самого анализатора/`x/tools` на Go 1.27.3; не засчитано как результат безопасности приложения. |
| `go run golang.org/x/vuln/cmd/govulncheck@latest ./...` | Загружена v1.9.0; `No vulnerabilities found.` |
| `docker compose -p kimi-review-booking up --build -d` | Multi-stage build успешен; БД healthy, `GET /healthz` приложения вернул `200`. |
| Отдельный `docker run ... --name kimi-review-booking-app-2 --network kimi-review-booking_default -p 8081:8080 ...` | Второй реальный контейнер приложения подключился к общей БД; `/healthz` на 8080 и 8081 вернул `200`. |
| `python3 /tmp/opencode/kimi-booking-review/two_instances.py` | 64 конкурентных запроса на места: 6×201, 58×409, 18 активных мест, дублей нет. 32 одинаковых idempotent-запроса: один booking id и одна строка. 32 одновременных cancel: все 200, один `cancelled_at`, активных мест 0, повторное бронирование успешно. 20 гонок cancel/rebook: дублей активных мест нет. Полный текст ниже. |
| `docker compose -p kimi-review-booking restart app`, `docker restart kimi-review-booking-app-2`, затем `cmp` ответов до/после | После реального перезапуска обоих приложений ответы `GET /events` и `GET /bookings/65` побайтово не изменились. |
| Повтор create с прежним ключом и повтор cancel после перезапуска | `200`, прежнее бронирование 65 со статусом `cancelled`; повторная отмена не изменила `cancelled_at`. |
| Unicode-boundary запросы к `/events` и `/bookings` | Подтверждён P2: 200/128 ASCII символов приняты, такое же число кириллических символов отклонено. |
| Percent-encoded newline в URL и чтение `docker logs` | Подтверждена P3-подделка многострочного журнала. |
| `GET /does-not-exist`, `DELETE /events` | Подтверждены text/plain 404/405 вместо документированного JSON. |
| JSON-тело больше 1 МиБ | `400 invalid_request`, `http: request body too large`. |
| SQL-shaped строка в имени события | Строка сохранена как данные; `GET /events` продолжил работать, SQL-инъекции не произошло. |
| `docker inspect` пользователя и `docker ps` портов | Приложение запущено как `app`; host-порт опубликован только у HTTP, у PostgreSQL host-порта нет. |
| `docker rm -f kimi-review-booking-app-2`; `docker compose -p kimi-review-booking down -v` | Удалены только созданные ревью контейнеры, сеть и volume. |
| `git diff --exit-code`, `git status --short` до создания отчёта | Исходное дерево оставалось чистым; реализация не менялась. |

## Не выполненные проверки и ограничения

- **Нагрузочные атаки, fuzzing и длительный soak-тест не выполнялись** — они выходят за разрешённую лёгкую локальную проверку; поэтому пределы ресурсов и отсутствие редких race conditions не доказаны.
- **Fault injection (`kill -9`, отключение БД посреди commit, сетевые разрывы, disk full) не выполнялся.** Проверен обычный реальный перезапуск приложений, но не восстановление после аварийных инфраструктурных отказов.
- **ОС-пакеты и базовые контейнерные images отдельным scanner не проверялись.** `govulncheck` покрывает Go, а не Alpine/PostgreSQL image целиком.
- **TLS, публичная аутентификация и production-secret management не проверялись**, поскольку не входят в `TASK.md`; выше они указаны как условия публичного развёртывания.
- Проверки выполнены на Linux/amd64, Go 1.27.3, Docker 29.9.2, Compose 5.7.0 и актуальном на момент запуска `postgres:16-alpine`; другие платформы и версии не тестировались.

## Сильные стороны решения

- Основные гарантии опираются на ограничения и транзакции PostgreSQL, а не на локальные mutex или надежду на удачное планирование goroutines.
- Многоместное бронирование атомарно; повторный ключ и отмена сохраняются после перезапуска и работают через разные экземпляры.
- Штатные integration-тесты действительно используют PostgreSQL, race detector и проверяют как HTTP-ответы, так и DB-инварианты.
- Миграции имеют advisory lock, транзакционны и безопасно повторяются на заполненной базе.
- README подробно описывает API, идемпотентность, компромиссы и ограничения; команды запуска и тестирования воспроизводятся.
- Есть разумные базовые меры безопасности: параметризованный SQL, строгий JSON, лимит тела, HTTP-таймауты, непривилегированный пользователь контейнера и внутренняя сеть БД.

## Приложение — временный независимый тест двух экземпляров

Файл создан вне проверяемого проекта: `/tmp/opencode/kimi-booking-review/two_instances.py`.

Предусловия воспроизведения: собственный Compose-проект запущен на 8080, второй контейнер — на 8081, контейнер БД называется `kimi-review-booking-db-1`.

Команда:

```bash
python3 /tmp/opencode/kimi-booking-review/two_instances.py
```

Полный текст:

```python
#!/usr/bin/env python3
import concurrent.futures
import json
import random
import subprocess
import urllib.error
import urllib.request

BASES = ["http://localhost:8080", "http://localhost:8081"]


def request(method, path, body=None, instance=0):
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(
        BASES[instance % 2] + path,
        data=data,
        method=method,
        headers={"Content-Type": "application/json"} if data else {},
    )
    try:
        with urllib.request.urlopen(req, timeout=20) as response:
            return response.status, json.load(response)
    except urllib.error.HTTPError as error:
        return error.code, json.load(error)


def create_event(name, capacity):
    status, event = request("POST", "/events", {"name": name, "capacity": capacity})
    assert status == 201, (status, event)
    return event


def book(event_id, token, seats, instance):
    return request("POST", "/bookings", {
        "event_id": event_id,
        "idempotency_key": token,
        "seats": seats,
    }, instance)


def sql(query):
    return subprocess.check_output([
        "docker", "exec", "kimi-review-booking-db-1",
        "psql", "-U", "booking", "-d", "booking", "-Atc", query,
    ], text=True).strip()


# Independent oversell check through two real application containers.
event = create_event("review-two-containers", 24)
rng = random.Random(4711)
seat_sets = [rng.sample(range(1, 25), 3) for _ in range(64)]
with concurrent.futures.ThreadPoolExecutor(max_workers=32) as pool:
    results = list(pool.map(
        lambda item: book(event["id"], f"review-over-{item[0]}", item[1], item[0]),
        enumerate(seat_sets),
    ))
assert all(status in (201, 409) for status, _ in results), results
active, distinct = sql(
    f"SELECT count(*), count(DISTINCT seat_no) FROM booking_seats "
    f"WHERE event_id={event['id']} AND active"
).split("|")
assert active == distinct and int(active) <= 24, (active, distinct)
print("oversell:", {status: sum(s == status for s, _ in results) for status in (201, 409)},
      "active=", active)

# Concurrent identical retries must produce one booking and one response ID.
idem_event = create_event("review-idempotency", 4)
with concurrent.futures.ThreadPoolExecutor(max_workers=16) as pool:
    idem = list(pool.map(lambda i: book(idem_event["id"], "review-same-key", [1, 2], i), range(32)))
assert all(status in (200, 201) for status, _ in idem), idem
ids = {body["id"] for _, body in idem}
assert len(ids) == 1, ids
booking_id = ids.pop()
assert sql(f"SELECT count(*) FROM bookings WHERE event_id={idem_event['id']} "
           "AND client_token='review-same-key'") == "1"
print("idempotency: responses=", len(idem), "booking_id=", booking_id)

# Concurrent cancellation through both containers; then seat reuse.
with concurrent.futures.ThreadPoolExecutor(max_workers=16) as pool:
    cancelled = list(pool.map(
        lambda i: request("POST", f"/bookings/{booking_id}/cancel", instance=i), range(32)
    ))
assert all(status == 200 and body["status"] == "cancelled" for status, body in cancelled), cancelled
assert len({body["cancelled_at"] for _, body in cancelled}) == 1
assert sql(f"SELECT count(*) FROM booking_seats WHERE booking_id={booking_id} AND active") == "0"
status, rebooked = book(idem_event["id"], "review-after-cancel", [1, 2], 1)
assert status == 201, (status, rebooked)
print("cancel: responses=", len(cancelled), "cancelled_at values=1; rebook=201")

# Repeated cancel/rebook races on separate instances.
for i in range(20):
    race_event = create_event(f"review-race-{i}", 4)
    status, old = book(race_event["id"], f"review-race-old-{i}", [1, 2, 3], 0)
    assert status == 201
    with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
        cancel_future = pool.submit(request, "POST", f"/bookings/{old['id']}/cancel", None, 0)
        book_future = pool.submit(book, race_event["id"], f"review-race-new-{i}", [2], 1)
        cancel_result = cancel_future.result()
        book_result = book_future.result()
    assert cancel_result[0] == 200, cancel_result
    assert book_result[0] in (201, 409), book_result
assert sql("SELECT count(*) FROM (SELECT event_id, seat_no FROM booking_seats "
           "WHERE active GROUP BY event_id, seat_no HAVING count(*) > 1) d") == "0"
print("cancel/rebook races: 20; duplicate active seats=0")
```

Фактический вывод:

```text
oversell: {201: 6, 409: 58} active= 18
idempotency: responses= 32 booking_id= 65
cancel: responses= 32 cancelled_at values=1; rebook=201
cancel/rebook races: 20; duplicate active seats=0
```
