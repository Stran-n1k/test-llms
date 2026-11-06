# Независимое ревью

## Заключение

По результатам чтения кода и успешных модульных, интеграционных и сквозных проверок решение соответствует `TASK.md` в проверенной части. Реализованы ограничение вместимости, атомарное бронирование нескольких мест, идемпотентность по обязательному `Idempotency-Key`, однократный возврат мест при отмене, координация двух экземпляров через PostgreSQL, валидация и повторяемые миграции.

**Подтверждённых дефектов не найдено.** Замечаний с приоритетами P0–P3 нет. Помимо автоматического набора, успешно завершены полный запуск `compose.yaml` с двумя контейнерами приложения, независимые конкурентные HTTP-проверки и остановка всего стека с последующим запуском на том же named volume. Данные, схема и идемпотентность после перезапуска сохранились.

## Проверка требований

- **Сохранность данных:** `test/e2e/restart_test.go:24-66` проверяет `SIGKILL`, повторный запуск процесса на той же базе, сохранение бронирования, счётчика мест и ключа идемпотентности. Дополнительно через Compose были остановлены оба приложения и PostgreSQL, после чего весь стек запущен на прежнем named volume. Созданное до остановки событие и бронирование прочитаны без изменений, повтор с прежним ключом вернул то же бронирование и не занял новые места. Обе проверки прошли.
- **Отсутствие перепродажи:** проверка остатка и изменение счётчика находятся в одной транзакции; строка события блокируется через `FOR NO KEY UPDATE` (`internal/repository/postgres/booking.go:27-81`). Диапазон счётчика дополнительно ограничен `CHECK` (`internal/repository/postgres/migrations/0001_init.sql:9-12`). Конкурентные проверки на репозитории и на HTTP через два процесса прошли.
- **Идемпотентность:** глобальный `UNIQUE` на ключе (`internal/repository/postgres/migrations/0001_init.sql:16-30`) сочетается с проверкой тождественности события, нормализованного имени и числа мест (`internal/repository/postgres/booking.go:90-108`). Повторы, конфликт ключа с другим запросом, повтор после отмены и конкуренция одного ключа на разных событиях покрыты тестами; все проверки прошли.
- **Повторная и конкурентная отмена:** условный `UPDATE ... WHERE status = 'confirmed'` и возврат мест выполняются в одной транзакции (`internal/repository/postgres/booking.go:157-197`). Повторные и 40 одновременных отмен через два процесса вернули места один раз.
- **Два экземпляра:** сквозной набор поднимает два процесса с отдельными пулами на общей PostgreSQL (`test/e2e/suite_test.go:89-117`) и проверяет бронирования, повторы и отмены через оба процесса. Затем те же сценарии независимо повторены через два контейнера Compose на портах 28080 и 28081: 100 конкурентных заявок по 2 места дали ровно 15 успехов и 85 отказов при вместимости 30; 40 запросов с одним ключом дали одно бронирование; 40 конкурентных отмен вернули места один раз. Все проверки прошли.
- **Валидация:** строгий JSON с запретом неизвестных полей и лишних значений, ограничение тела 1 МиБ (`internal/handler/rest/decode.go:13-44`), проверки текста, чисел, дат и пагинации реализованы и покрыты тестами. Ошибки имеют стабильные машиночитаемые коды.
- **Схема и повторный запуск:** миграции встроены в бинарник, выполняются в транзакции под advisory lock и сверяют SHA-256 уже применённых файлов (`internal/repository/postgres/migrate.go:24-100`). Пустая база, повтор с сохранением данных, восемь конкурентных запусков и отказ при изменении применённой миграции проверены на PostgreSQL.
- **Автоматические тесты:** тесты проверяют не только счётчик, но и его равенство сумме подтверждённых бронирований (`internal/repository/postgres/repository_integration_test.go:171-187`). Есть смешанная конкуренция создания и отмены, гонка ключа между разными событиями и ожидание незакоммиченного ключа. Это содержательные проверки инвариантов, а не только проверки успешных статусов.

## Архитектура

### Оценка

Архитектура соразмерна задаче и не требует изменений. Направление зависимостей последовательное: HTTP-адаптер вызывает сервисы, сервисы зависят от узких интерфейсов репозиториев, PostgreSQL-реализация и composition root находятся снаружи (`internal/handler/rest/dependency.go:15-34`, `internal/service/booking/dependency.go:14-21`, `internal/app/app.go:61-67`). Доменные модели не зависят от HTTP или `pgx`.

Бизнес-инварианты размещены удачно:

- нормализация и проверка входных команд — в сервисах;
- атомарность, блокировки и согласованность `booked_seats` с бронированиями — в репозитории, непосредственно у транзакций;
- последняя защита диапазона счётчика и уникальности ключа — в схеме базы.

Такое размещение важнее формального числа слоёв: перенос конкурентных правил только в сервис не обеспечил бы координацию двух процессов, а здесь границы транзакций совпадают с инвариантами.

HTTP-сервер имеет отдельную сборку middleware, единый маппинг ошибок и явные таймауты (`internal/app/app.go:28-32,74-80`). Запуск, миграции, ожидание базы и graceful shutdown собраны в одном composition root, без размазывания инфраструктурных обязанностей по бизнес-логике.

Тестируемость высокая: интерфейсы позволяют изолированно проверять HTTP и сервисы, а критические правила дополнительно проверяются без моков на PostgreSQL и через настоящий бинарник. Это правильный баланс: основная уверенность получена не от моков, а от интеграционных и сквозных сценариев.

### Компромиссы и стоимость изменений

- `CreateServiceCommand` и `CreateRepositoryCommand` для события и бронирования сейчас структурно повторяются, а OpenAPI и generated mocks добавляют заметный объём вспомогательного кода. Для двух сущностей это небольшая цена за явные границы и проверяемый контракт; объединять слои только ради сокращения числа файлов не требуется.
- Счётчик `events.booked_seats` денормализован. Он даёт простую блокировку вместимости, но каждый новый путь записи обязан сохранять его равенство сумме подтверждённых бронирований. Сейчас запись сосредоточена в одном репозитории, а инвариант проверен тестами. `CHECK` ограничивает диапазон, но не доказывает равенство сумме; это архитектурный компромисс, а не текущий дефект.
- Блокировка строки события сериализует записи по одному популярному событию. Это простая и надёжная реализация для небольшой задачи; цена — ограниченный параллелизм и возможные 503 при длительной очереди, что честно описано в `README.md:213-215`. Более сложная схема без подтверждённой нагрузочной необходимости не оправдана.
- Собственный небольшой механизм миграций достаточен для текущей одной транзакционной миграции. При появлении нетранзакционных DDL-операций, обратных миграций или сложной эволюции схемы его возможности придётся пересмотреть; это не влияет на выполнение текущего задания.

## Безопасность

### Модель угроз и границы доверия

Рассматривались недоверенные HTTP-клиенты и их путь, query, заголовки и JSON; приложение и его конфигурация считаются доверенными, PostgreSQL — отдельной доверенной зависимостью. Отсутствие авторизации прямо разрешено заданием и не считается дефектом. Активные запуски выполнялись только против локальных проверяемых процессов и собственных контейнеров, без внешних целей и нагрузочных атак.

### Результаты проверки

**Подтверждённых уязвимостей в рамках этой модели не найдено.** Это не означает доказанную безопасность решения.

- **SQL-инъекции:** значения HTTP-запросов передаются параметрами `$1`, `$2` и т. д.; конкатенируются только константные списки столбцов. Миграции берутся из встроенных файлов, а не из пользовательского ввода. Динамические имена временных баз встречаются только в тестах и цитируются. Через запущенный Compose-сервис создано событие с именем `x'); DROP TABLE events;--`: оно сохранилось как обычная строка, читалось после полного перезапуска, таблица событий осталась доступной.
- **Недоверенный JSON:** применяются `DisallowUnknownFields`, проверка единственного JSON-значения, строгие типы generated models и `http.MaxBytesReader` с пределом 1 МиБ. Текст ограничен 200 символами после нормализации, управляющие символы отклоняются, ключ — 255 байтами printable ASCII. Числа и UUID проверяются до обращения к базе.
- **Таймауты и ресурсы:** есть 10-секундный контекст запроса, `ReadHeaderTimeout`, `ReadTimeout`, `WriteTimeout`, `IdleTimeout`, предел размера страницы 200 и предел тела. Ожидание блокировок и получение соединения из пула ограничено контекстом запроса. Стандартный `net/http` также имеет предел заголовков по умолчанию; отдельный предел числа одновременных клиентов не настроен, но подтверждённого исчерпания ресурсов проверками не получено.
- **Ошибки и секреты:** неожиданные ошибки и panic возвращают клиенту только общий `internal_error`; подробности остаются в журнале. Журнал запросов содержит метод, шаблон маршрута, статус и длительность, но не тело, query или `Idempotency-Key`. Отдельный запуск с учебным паролем в некорректном `DATABASE_URL` показал, что `pgx` заменяет пароль на `xxxxx`; учебный секрет в журнал не попал.
- **Контейнеры и порты:** приложение работает от UID 10001 (`Dockerfile:10-16`), PostgreSQL не публикует порт на хост (`compose.yaml:20-34`). Порты HTTP приложения опубликованы; в учебной конфигурации используются общие пароль `booking` и `sslmode=disable`, что явно оговорено в `README.md:218-220`.

### Условия публичной эксплуатации

Для развёртывания вне локальной учебной среды нужны отдельные эксплуатационные меры: терминация TLS доверенным proxy, секретное хранение и передача `DATABASE_URL`, TLS к PostgreSQL при недоверенной сети и ограничение доступа к опубликованным HTTP-портам. Сейчас любой клиент может читать и отменять любые бронирования; это прямо разрешено заданием, но недопустимо для системы с разграничением владельцев данных. По той же причине ключи идемпотентности должны быть непредсказуемыми: знание ключа и полного совпадающего запроса позволяет получить существующее бронирование, как и описано в README.

## Неподтверждённые риски

- Устойчивость к длительной высокой нагрузке, большому числу одновременных TCP-соединений и исчерпанию пула не измерялась. Тесты подтверждают корректность при десятках конкурентных запросов, а не предел производительности.
- Известные уязвимости зависимостей и базовых образов не проверялись сканерами. Поэтому нельзя утверждать, что уязвимости в зависимостях отсутствуют; статическая проверка затронутого кода такого вывода не даёт.

## Необязательные рекомендации

1. Для более строгой воспроизводимости зафиксировать базовые Docker-образы по digest. Текущие конкретные версии тегов не являются ошибкой задания.
2. В CI периодически запускать `govulncheck` и сканер образов, оценивая найденное применительно к фактически вызываемому коду.
3. Для неучебного развёртывания рассмотреть `read_only`, `cap_drop: [ALL]`, `no-new-privileges` и лимиты ресурсов контейнеров. Это дополнительное усиление, а не требование `TASK.md`.

## Выполненные проверки

Исходники, штатные тесты, зависимости, `TASK.md` и `README.md` не изменялись. Для независимой проверки Compose создан временный `/tmp/opencode/kimi_compose_check.py`; его текст и команды воспроизведения приведены ниже.

1. **`go test -race ./...`** — успех. Прошли все пакеты; интеграционный пакет PostgreSQL — примерно за 41 с, сквозной — примерно за 40 с. Testcontainers запускал настоящую `postgres:18.1-alpine`; сквозные тесты собрали бинарник и запустили два процесса на общей базе. Race detector ошибок не сообщил.
2. **`go vet ./...`** — успех, без вывода.
3. **`DATABASE_URL='postgres://booking:dummy-secret@%' HTTP_ADDR='127.0.0.1:0' go run ./cmd/booking`** — ожидаемый отказ запуска из-за некорректного URL; в сообщении пароль замаскирован как `xxxxx`, `dummy-secret` отсутствует.
4. **`APP_PORT=28080 APP2_PORT=28081 docker compose -p kimi-review-b642dd6 --profile two-instances config --quiet`** — успех, конфигурация обоих экземпляров валидна.
5. **Первый запуск той же конфигурации через `up --build --wait`** — Dockerfile для обоих приложений собрался, но PostgreSQL не инициализировался: `initdb: ... No space left on device`. `docker compose ... ps -a`, `docker compose ... logs --no-color --tail=80` и `df -h` подтвердили заполнение корневой ФС на 100%, а не ошибку приложения. Собственные частично созданные ресурсы были удалены.
6. **`docker image prune --all --force`** — после явного разрешения пользователя удалены неиспользуемые образы, включая чужие; освобождено 1,529 ГБ. Команда не удаляет контейнеры и тома. Это была очистка окружения, а не проверка приложения.
7. **Повторный `APP_PORT=28080 APP2_PORT=28081 docker compose -p kimi-review-b642dd6 --profile two-instances up --build --wait`** — успех. PostgreSQL и оба приложения перешли в `healthy`; порты приложений 28080/28081 опубликованы, порт PostgreSQL опубликован только внутри сети Compose.
8. **`python3 /tmp/opencode/kimi_compose_check.py load`** — успех: независимые HTTP-проверки перепродажи, идемпотентности, конкурентной отмены, строгого JSON, предела 1 МиБ и SQL-инъекционной строки прошли через оба контейнера.
9. **`docker compose ... stop app app2`, затем `docker compose ... stop postgres`, затем `APP_PORT=28080 APP2_PORT=28081 docker compose -p kimi-review-b642dd6 --profile two-instances up --no-build --wait`** — успех, все три контейнера снова `healthy` на прежнем volume; приложения повторно выполнили миграции.
10. **`python3 /tmp/opencode/kimi_compose_check.py verify`** — успех: событие, бронирование, счётчик и SQL-инъекционная строка сохранились; повтор с прежним ключом после полного перезапуска вернул `200`, `Idempotent-Replayed: true` и исходное бронирование без изменения мест.
11. **`docker compose ... logs --no-color --since=2m`** — журналы проверочного периода просмотрены; записей `panic`, `level=ERROR` и утечки проверочного секрета не найдено.
12. **`git status --short`** — до создания отчёта рабочее дерево было чистым; после него добавлен только `kimi_review.md`.

После проверок собственные контейнеры, сеть и named volume проекта `kimi-review-b642dd6` удалены. Чужие контейнеры и тома не удалялись; чужие неиспользуемые образы удалялись только по явному разрешению, как описано в пункте 6.

## Не выполненные проверки и ограничения

- Сканирование зависимостей и образов на известные уязвимости не выполнялось.
- Нагрузочное, fuzz- и долговременное soak-тестирование не проводилось; независимые параллельные сценарии проверяют корректность, а не производительность.
- Внешние цели, тяжёлые нагрузочные атаки и проверки TLS/авторизации не выполнялись: они выходят за модель и ограничения этого ревью.

## Сильные стороны

- Инварианты обеспечены транзакциями и ограничениями PostgreSQL, а не только последовательностью вызовов в памяти одного процесса.
- Идемпотентность продумана для конкуренции, разных событий, отменённого бронирования и неоднозначного результата после обрыва связи.
- Тесты воспроизводят сложные гонки и проверяют согласованность данных, включая смешанные создания и отмены.
- Контракт API, README и реализация согласованы; допущения и ограничения описаны конкретно.
- Обработка ввода, ошибок, таймаутов и запуска аккуратна; приложение в образе не работает от root.

## Приложение: временная проверка Compose

Воспроизведение из корня проекта (скрипт ниже сохранить как `/tmp/opencode/kimi_compose_check.py`):

```bash
APP_PORT=28080 APP2_PORT=28081 docker compose -p kimi-review-b642dd6 --profile two-instances up --build --wait
python3 /tmp/opencode/kimi_compose_check.py load
APP_PORT=28080 APP2_PORT=28081 docker compose -p kimi-review-b642dd6 --profile two-instances stop app app2
APP_PORT=28080 APP2_PORT=28081 docker compose -p kimi-review-b642dd6 --profile two-instances stop postgres
APP_PORT=28080 APP2_PORT=28081 docker compose -p kimi-review-b642dd6 --profile two-instances up --no-build --wait
python3 /tmp/opencode/kimi_compose_check.py verify
```

Текст использованного скрипта:

```python
#!/usr/bin/env python3
import json
import sys
import urllib.error
import urllib.request
import uuid
from collections import Counter
from concurrent.futures import ThreadPoolExecutor

BASES = ["http://127.0.0.1:28080", "http://127.0.0.1:28081"]
STATE = "/tmp/opencode/kimi-compose-state.json"


def call(base, method, path, body=None, key=None, content_type="application/json"):
    data = body if isinstance(body, bytes) else (json.dumps(body).encode() if body is not None else None)
    headers = {"Content-Type": content_type} if data is not None else {}
    if key is not None:
        headers["Idempotency-Key"] = key
    request = urllib.request.Request(base + path, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(request, timeout=20) as response:
            raw = response.read()
            return response.status, response.headers, json.loads(raw) if raw else None
    except urllib.error.HTTPError as error:
        raw = error.read()
        return error.code, error.headers, json.loads(raw) if raw else None


def event(base, capacity, name="Concert"):
    status, _, body = call(base, "POST", "/events", {
        "name": name, "starts_at": "2030-01-31T19:00:00Z", "capacity": capacity,
    })
    assert status == 201, (status, body)
    return body


def book(index, event_id, key, seats, name="Anna"):
    return call(BASES[index % 2], "POST", f"/events/{event_id}/bookings",
                {"customer_name": name, "seats": seats}, key)


def parallel(count, operation):
    with ThreadPoolExecutor(max_workers=count) as executor:
        return list(executor.map(operation, range(count)))


def bookings(event_id):
    result, offset = [], 0
    while True:
        status, _, page = call(BASES[0], "GET", f"/events/{event_id}/bookings?limit=200&offset={offset}")
        assert status == 200, (status, page)
        result.extend(page["items"])
        if not page["has_more"]:
            return result
        offset = len(result)


def get_event(index, event_id):
    status, _, body = call(BASES[index], "GET", f"/events/{event_id}")
    assert status == 200, (status, body)
    return body


def load():
    crowded = event(BASES[0], 30)
    answers = parallel(100, lambda i: book(i, crowded["id"], str(uuid.uuid4()), 2))
    assert Counter(a[0] for a in answers) == {201: 15, 409: 85}
    assert get_event(1, crowded["id"])["booked_seats"] == 30
    assert len(bookings(crowded["id"])) == 15

    same = event(BASES[0], 10)
    key = str(uuid.uuid4())
    answers = parallel(40, lambda i: book(i, same["id"], key, 4))
    assert Counter(a[0] for a in answers) == {201: 1, 200: 39}
    assert len({a[2]["id"] for a in answers}) == 1
    assert all(a[1].get("Idempotent-Replayed") == "true" for a in answers if a[0] == 200)
    assert get_event(1, same["id"])["booked_seats"] == 4

    cancelling = event(BASES[0], 10)
    assert book(0, cancelling["id"], str(uuid.uuid4()), 4)[0] == 201
    status, _, target = book(0, cancelling["id"], str(uuid.uuid4()), 3)
    assert status == 201
    answers = parallel(40, lambda i: call(BASES[i % 2], "POST", f"/bookings/{target['id']}/cancel"))
    assert all(a[0] == 200 and a[2]["status"] == "cancelled" for a in answers)
    assert len({a[2]["cancelled_at"] for a in answers}) == 1
    assert get_event(1, cancelling["id"])["booked_seats"] == 4

    status, _, problem = call(BASES[0], "POST", "/events", b'{"unknown":true}')
    assert status == 400 and problem["error"]["code"] == "invalid_json"
    oversized = b'{"name":"' + b'a' * (1 << 20) + b'","starts_at":"2030-01-31T19:00:00Z","capacity":1}'
    status, _, problem = call(BASES[0], "POST", "/events", oversized)
    assert status == 413 and problem["error"]["code"] == "payload_too_large"

    injected_name = "x'); DROP TABLE events;--"
    injected = event(BASES[0], 1, injected_name)
    assert get_event(1, injected["id"])["name"] == injected_name

    persistent = event(BASES[0], 7)
    persistent_key = str(uuid.uuid4())
    status, _, created = book(0, persistent["id"], persistent_key, 3)
    assert status == 201
    with open(STATE, "w") as output:
        json.dump({"event": persistent, "booking": created, "key": persistent_key,
                   "injected_id": injected["id"], "injected_name": injected_name}, output)
    print("load: oversell, idempotency, cancellation, validation and injection checks passed")


def verify():
    with open(STATE) as source:
        state = json.load(source)
    current = get_event(1, state["event"]["id"])
    assert current["booked_seats"] == 3 and current["available_seats"] == 4
    status, _, found = call(BASES[1], "GET", f"/bookings/{state['booking']['id']}")
    assert status == 200 and found == state["booking"]
    status, headers, replay = book(0, state["event"]["id"], state["key"], 3)
    assert status == 200 and headers.get("Idempotent-Replayed") == "true"
    assert replay == state["booking"] and get_event(1, state["event"]["id"])["booked_seats"] == 3
    assert get_event(0, state["injected_id"])["name"] == state["injected_name"]
    print("verify: data, idempotency and schema survived full stack restart")


if __name__ == "__main__":
    {"load": load, "verify": verify}[sys.argv[1]]()
```
