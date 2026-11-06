# Независимое техническое ревью

Дата: 6 ноября 2026 года. Проверен commit `89ace61f21369bb30f4722194181ab379904eb52`.
Прочитаны `../REVIEW_INSTRUCTIONS.md`, `TASK.md`, `README.md`, все исходники,
тесты, SQL, Dockerfile, Compose и Makefile. Реализация, зависимости и штатные
тесты не изменялись. Собственные проверки находятся в `/tmp/codex-booking-review`;
их полный текст приведён в приложениях. Другие агенты и готовые решения не использовались.

## Заключение о соответствии TASK.md

Решение в основном соответствует заданию. На настоящей PostgreSQL подтверждены
сохранность данных, отсутствие перепродажи, отсутствие дубликатов бронирований,
однократное освобождение мест при повторной и конкурентной отмене, работа двух
независимых процессов, подготовка пустой схемы и повторный запуск. Dockerfile
собирается, Compose запускается; пересоздание контейнеров с сохранением тома
сохраняет данные и ключи идемпотентности. Все штатные тесты проходят, в том числе
с `-race`.

Подтверждены **два дефекта P2 и один P3**. Конкурентный повтор на последние места
может получить ложный отказ вместо документированного replay. Валидация
пропускает NUL в именах, после чего клиент получает непонятную внутреннюю ошибку.
Есть небольшая неточность классификации слишком большого JSON-тела. Первые два
замечания затрагивают безопасное повторение запросов и требование понятных ошибок;
третье касается описанного автором HTTP-контракта. Подтверждённых P0/P1, потери
данных или перепродажи не обнаружено.

## Подтверждённые дефекты

### 1. P2 — конкурентный повтор получает `not_enough_seats`, хотя его бронирование создано

**Место:** `internal/store/store.go:202–223, 224–244`;
обработка гонки через уникальность расположена позже, в `270–285`.

**Условие:** два запроса с одинаковыми событием, ключом и телом прочитали отсутствие
бронирования до фиксации первого. После первого бронирования свободных мест уже
меньше, чем запрошено вторым. Например, вместимость 1, оба запроса бронируют 1 место.

**Причина:** оба запроса проходят начальную проверку ключа. Первый делает `UPDATE`
и создаёт запись. Второй после ожидания блокировки не проходит условие ёмкости
и сразу возвращает `NotEnoughSeatsError`. До `INSERT`, чья ошибка уникальности
преобразуется в replay, выполнение не доходит. В этой ветви ключ повторно не читается.

**Последствие:** клиент видит отказ по операции, которая уже успешно выполнена под
его ключом. Это нарушает обещанный в README результат безопасного повтора
`200` с тем же бронированием, особенно при повторной отправке до получения
первоначального ответа. Дубликата и перепродажи при этом нет; обычный последующий
повтор после завершения гонки возвращает `200`.

**Доказательство:** приложение A. Внешняя транзакция блокирует строку события;
два HTTP-запроса к двум процессам доходят до ожидания `UPDATE`, что проверяется
через `pg_stat_activity`. После снятия блокировки фактически получены:

```text
expected [200, 201], got [201, 409]
409 {"error":{"code":"not_enough_seats", ...,
              "details":{"available_seats":0,"requested_seats":1}}}
```

В БД остались ровно одна запись и одно занятое место. Штатный
`TestConcurrentIdempotentCreate` (`internal/integration/api_test.go:462–515`)
использует ёмкость 10 и запрос на 3 места: проигравшие транзакции могут временно
зарезервировать ещё 3 места и дойти до уникального индекса. Поэтому он не покрывает
эту границу.

**Направление исправления:** разрешать гонку ключа независимо от остатка ёмкости.
Например, перед возвратом нехватки мест повторно проверять уже зафиксированное
бронирование и его hash либо сериализовать обработку ключа до проверки ёмкости.
Добавить детерминированный тест с недостаточной ёмкостью для двух временных резервов.

### 2. P2 — NUL в имени проходит валидацию и превращается в HTTP 500

**Место:** `internal/api/validation.go:49–57, 84–88`;
запись строк в PostgreSQL — `internal/store/store.go:85–94, 258–287`;
обобщение ошибки — `internal/api/errors.go:78–84`.

**Условие:** клиент передаёт JSON с `"name":"bad\u0000name"` при создании события
либо `"customer_name":"bad\u0000name"` при бронировании. JSON синтаксически
корректен; эти значения нельзя сохранить в используемых столбцах PostgreSQL `text`.

**Последствие:** запрос проходит HTTP-валидацию, затем получает `500 internal_error`
без указания проблемного поля. Клиент не может понять, как исправить запрос, и
может бесполезно повторять его как сбой сервера. Это противоречит требованию
понятных ошибок на некорректный ввод. Данные не повреждаются: неудачная вставка
бронирования откатывается вместе с резервированием мест.

**Доказательство:** оба запроса в приложении A вернули:

```json
{"error":{"code":"internal_error","message":"internal server error"}}
```

В собственных логах сервера зафиксировано:

```text
insert event: ERROR: invalid byte sequence for encoding "UTF8": 0x00 (SQLSTATE 22021)
insert booking: ERROR: invalid byte sequence for encoding "UTF8": 0x00 (SQLSTATE 22021)
```

Число событий не изменилось; для неудачной брони проверены ноль записей и ноль
занятых мест непосредственно в БД.

**Направление исправления:** отклонять NUL в принимаемых текстовых полях до SQL
с `400 validation_error` и названием поля. Нет необходимости запрещать все
не-ASCII символы или произвольно ужесточать допустимые имена.

### 3. P3 — превышение лимита после JSON-объекта ошибочно классифицируется как `invalid_json`

**Место:** `internal/api/errors.go:121–123` (сравнить с обработкой `MaxBytesError`
при первом чтении в `105–113`).

**Условие:** корректный небольшой JSON-объект дополнен пробелами так, что всё тело
превышает 1 MiB. Лимит срабатывает при втором `Decode`, проверяющем окончание тела.

**Последствие:** вместо описанных в README `413 payload_too_large` клиент получает
`400 invalid_json` и сообщение о необходимости одного объекта, хотя объект ровно
один. Лимит не обходится, запись не создаётся; это дефект ответа, а не подтверждённая
уязвимость исчерпания памяти.

**Доказательство:** приложение A отправляет корректный объект создания события плюс
`1 << 20` пробелов; получен `400`. Тело, превышающее лимит внутри первого объекта,
получает ожидаемый `413`.

**Направление исправления:** при втором чтении также распознавать `http.MaxBytesError`
и возвращать тот же `413 payload_too_large`.

## Архитектура

**Соразмерность и ответственность.** Для небольшой задачи структура разумна:
`cmd/server` собирает приложение и управляет жизненным циклом; `api` разбирает HTTP
и проверяет ввод; `store` содержит SQL и транзакции; `migrate` отвечает за схему.
Нет Redis, фоновых координаторов и лишних механизмов распределённой синхронизации.
Маленькие пакеты `config`, `db`, `model` добавляют немного навигации, но имеют ясные
границы; существенной избыточной сложности не обнаружено.

**Инварианты и транзакции.** В `store.CreateBooking` (`188–293`) резерв и вставка
брони атомарны. В `CancelBooking` (`302–347`) блокировка брони, смена статуса и
уменьшение счётчика находятся в одной транзакции. Блокировки PostgreSQL действуют
между процессами; локального источника истины нет. Ограничения схемы
(`0001_init.sql:11–15, 20–32`) дополнительно защищают ёмкость, статус, ссылочную
целостность и уникальность ключа. Это подходящее место для этих гарантий.
Исключение — порядок разрешения гонки ключа и проверки ёмкости в дефекте 1.

Счётчик `reserved_seats` ускоряет чтение доступности и даёт одну строку для
сериализации, но дублирует сумму активных броней. Равенство суммы и счётчика не
обеспечено межтабличным ограничением. В текущих путях приложения оно сохранено
транзакциями и подтверждено проверками. Цена будущего изменения статусов/удаления
броней — необходимость поддерживать этот инвариант во всех новых путях записи.
Это компромисс, а не найденное рассогласование.

**Зависимости и связанность.** Интерфейс хранилища объявлен у потребителя в
`internal/api/api.go:23–33`, что позволяет подставить тестовую реализацию без
сервера PostgreSQL. При этом сигнатуры используют `store.Create*Params`, а
HTTP-отображение ошибок знает `store.Err*`: слой API связан с пакетом конкретного
хранилища и транзитивно с его импортами. Для одного адаптера это допустимое
упрощение. При появлении другого хранилища или бизнес-операций вне HTTP перенос
контрактов и ошибок в нейтральный пакет уменьшил бы цену изменений; сейчас
обязательной перестройки слоёв не требуется.

**Тестируемость и стоимость изменений.** Валидация выделена в небольшие функции,
SQL и границы транзакций видны в одном месте. `scanEvent`/`scanBooking` и общие
списки столбцов уменьшают повторение. Создание брони длиннее остальных методов,
но ветви соответствуют реальным исходам операции. Штатные тесты изолируют схемы,
поднимают два независимых пула и HTTP-сервера и проверяют реальные ограничения
БД. Собственные проверки дополнительно использовали два отдельных OS-процесса.
Разделение HTTP-сервера и точки входа позволило проверить обе формы запуска.

**Схема.** Встроенные миграции и таблица версий воспроизводимы; session advisory
lock берётся на одном выделенном соединении (`migrate.go:35–58`), SQL и запись
версии выполняются одной транзакцией (`82–100`). Одновременный старт двух
процессов на пустой БД прошёл, применена одна версия. Текущая реализация не
хранит checksum старых миграций: при дальнейшем развитии применённые файлы
следует считать неизменяемыми. Это эксплуатационное соглашение, не дефект
нынешнего повторного запуска.

## Безопасность

### Модель угроз и границы доверия

Предполагаются недоверенные HTTP-клиенты, способные передавать произвольные JSON,
заголовки и пути и повторять запросы конкурентно. Оператор, конфигурация
`DATABASE_URL`, образ приложения и сама PostgreSQL считаются доверенными.
Проверялись границы HTTP → валидация → SQL, возврат ошибок и журналирование.
Все активные проверки выполнялись только против собственных временных сервисов
на localhost. Отсутствие авторизации разрешено TASK.md и не считается дефектом.

### Что проверено

- **SQL-инъекции.** Значения из HTTP передаются параметрами `$1`, `$2`, …;
  динамические части SQL приложения — постоянные списки столбцов. Строка
  `x'); DROP TABLE events; --` в имени клиента и ключе сохранилась как данные;
  таблица и счётчики остались работоспособны. Имена тестовых схем генерируются
  внутри `testutil`, а не берутся из HTTP. Подтверждённой SQL-инъекции не найдено.
- **Недоверенный JSON.** Требуется `application/json`, отклоняются неизвестные
  поля, дополнительное JSON-значение, неправильные типы, переполнение `int`,
  отсутствующие обязательные значения. Проверены битое тело, `null`, `{}{}`,
  дробная вместимость и переполнение числа. Валидация ограничивает количество
  мест, ёмкость, имена, email и ключ; UUID разбирается до SQL. Подтверждённый
  пробел в обработке NUL описан в дефекте 2.
- **Объём входа и таймауты.** Есть `MaxBytesReader` на 1 MiB, ограничение заголовков
  1 MiB, `ReadHeaderTimeout=5s`, `ReadTimeout=15s`, `WriteTimeout=30s`,
  `IdleTimeout=60s` (`cmd/server/main.go:51–59`). Контекст запроса ограничен 15s
  (`api/middleware.go:12–17`), readiness — 2s. Один незавершённый HTTP-запрос
  действительно остановлен через **15.02s**. Пул ограничен 10 соединениями на
  процесс (`db.go:21`). Тело сверх лимита отвергается; уточнение статуса — дефект 3.
- **Ошибки и секреты.** Ответы на ошибки БД содержат только `internal_error`;
  это проверено для NUL и остановки собственной БД. SQLSTATE и внутренние
  сообщения остаются в серверном логе. Обычный access-log содержит метод,
  путь, статус, размер и длительность (`middleware.go:47–58`), без тела,
  `Idempotency-Key`, email и `DATABASE_URL`. Panic recovery пишет stack trace
  только в серверный журнал. В проверенных ответах утечек секретов не найдено;
  все возможные сообщения стороннего драйвера отдельно не перебирались.
- **Контейнеры и сеть.** Многоэтапная сборка заканчивается distroless-образом,
  приложение работает от `nonroot:nonroot` (`Dockerfile:19–26`), что также
  подтверждено `docker inspect`. Privileged-режима, host network и Docker socket
  в Compose нет. В исходном Compose опубликованы оба порта на всех интерфейсах,
  задан известный dev-пароль и `sslmode=disable` (`compose.yaml:7–11, 25–28`).
  README прямо обозначает эти настройки как локальные. При ревью порты собственных
  сервисов ограничены `127.0.0.1`.

Подтверждённых эксплойтов SQL-инъекции, утечки секретов, обхода лимита тела или
нарушения учёта мест не получено. Найденные ошибки ввода и replay приведены выше;
отсутствие других находок не доказывает безопасность приложения.

### Условия публичного развёртывания — вне требований TASK.md

Исходный Compose нельзя считать готовой конфигурацией для недоверенной сети:
если хост доступен извне, опубликованный PostgreSQL и известные dev-реквизиты
дают прямой доступ к БД. Для такой эксплуатации нужны закрытый порт БД,
собственные секреты, ограниченные права DB-роли, TLS на соответствующих границах,
аутентификация/разграничение доступа на выбранном уровне и защита прокси от
избыточных запросов. Это условия развёртывания, а не требование добавить
авторизацию в данную задачу.

## Неподтверждённые риски и необязательные рекомендации

### Неподтверждённые риски

- Непагинированные `ListEvents`/`ListEventBookings` сначала собирают все строки
  в памяти (`store.go:112–130, 158–176`). При большом накопленном объёме повторное
  чтение списков может расходовать значительные память/CPU. README явно ограничивает
  сценарий малыми объёмами; OOM и деградация здесь не воспроизводились. При росте
  данных полезны пагинация и лимиты ответа.
- Лимит пула ограничивает число SQL-соединений, но не число ожидающих HTTP-запросов.
  При доступе множества недоверенных клиентов остаётся риск исчерпания соединений,
  goroutine и объёма логов. Нет экспериментального подтверждения отказа под
  нагрузкой; тяжёлые атаки не проводились. Лимиты конкурентности/частоты и ротация
  журналов уместны на границе развёртывания.

### Необязательные рекомендации

- У `encoding/json` здесь не добавлены отдельные правила для повторяющихся имён
  полей и ошибочных UTF-8 последовательностей в JSON-строках. Нет доказанного
  обхода бизнес-проверок через эти случаи. При подключении другого парсера на
  прокси или подписывании сырых тел стоит согласовать семантику разбора; это
  не дополнительное обязательное требование задания.
- Полезно дополнить тесты отрицательными сценариями размера тела, NUL,
  повтором после отмены, одновременным стартом миграций и недоступной БД.
  Их часть проверена внешними сценариями этого ревью, но в штатном наборе
  автоматической регрессии отсутствует. Отдельный тест гонки из дефекта 1 особенно
  важен, поскольку обычный успешный `-race` не проверяет логические гонки в БД.

## Фактически выполненные проверки

Рабочая папка — `/home/petr/review/project`. Для команд Go использованы только
временные кеши:

```bash
export GOCACHE=/tmp/codex-booking-review/gocache
export GOMODCACHE=/tmp/codex-booking-review/modcache
```

В таблице эти одинаковые присваивания опущены. Там, где нужны Docker, скачивание
зависимостей или localhost, команды выполнялись с разрешённым выходом из sandbox.

| Команда / проверка | Фактический результат |
| --- | --- |
| `git status --short`, `git diff --exit-code`, `git rev-parse HEAD` | До создания отчёта рабочее дерево чистое; commit указан в начале. В конце проверено, что единственный новый файл проекта — этот отчёт. |
| `go version` | `go1.27.3 linux/amd64`, соответствует `go.mod`. |
| `docker version` | Клиент/сервер 29.9.2. Первая попытка из sandbox не имела доступа к socket; после разрешённого выхода доступ получен. |
| `go test ./...` без временного кеша | Не начал сборку: основной Go build cache недоступен для записи. Это ограничение окружения. |
| `go test ./...` с временными кешами внутри sandbox | Не смог скачать модули из-за запрета DNS/сети. Не является ошибкой приложения. |
| Та же `go test ./...` с разрешённой сетью и Docker | **PASS**, API unit-тесты и все integration-тесты; настоящая PostgreSQL из Testcontainers, integration `15.638s`. |
| `docker run -d --name codex-review-booking-20261106 -e POSTGRES_USER=review -e POSTGRES_PASSWORD=review -e POSTGRES_DB=review -p 127.0.0.1::5432 postgres:16-alpine` | Собственная отдельная PostgreSQL запущена; случайно выделен host-порт `32768`. |
| `docker port codex-review-booking-20261106`; `docker exec codex-review-booking-20261106 pg_isready -U review -d review` | Порт привязан к localhost, БД принимает соединения. |
| `docker exec codex-review-booking-20261106 psql -X -qAt -U review -d review -c 'SELECT version()'` | PostgreSQL **16.15**, x86_64 Alpine. |
| `go build -o /tmp/codex-booking-review/server ./cmd/server` | **PASS**, исходный сервер собран без изменений. |
| `TEST_DATABASE_URL='postgres://review:review@127.0.0.1:32768/review?sslmode=disable' python3 /tmp/codex-booking-review/check.py` | Завершился с кодом 0; положительные инварианты проверены assertions, дефекты 1–3 напечатаны как наблюдения с ожидаемым и фактическим поведением. Код 0 здесь не означает отсутствие дефектов. Полный текст ниже. |
| `TEST_DATABASE_URL='postgres://review:review@127.0.0.1:32768/review?sslmode=disable' go test -race ./... -count=1` | **PASS**, все пакеты, integration `3.186s`, сообщений детектора гонок нет. |
| `go vet ./...` | **PASS**, код возврата 0, замечаний нет. |
| `go mod verify` | Сначала остановился на запрете сети sandbox для недостающих метаданных. После разрешённой повторной попытки — **`all modules verified`**. Это проверка целостности, не аудит уязвимостей. |
| `docker compose -p codex-review-booking-20261106 config --quiet` | **PASS**, синтаксис и конфигурация Compose принимаются. |
| `docker build -t codex-review-booking:20261106 .` | **PASS**, исходный Dockerfile полностью собран, включая загрузку зависимостей. |
| `APP_PORT=127.0.0.1:38080 POSTGRES_PORT=127.0.0.1:35432 docker compose -p codex-review-compose-20261106 up --build -d` | **PASS**, собственные сеть, том, app и PostgreSQL; оба сервиса работают, PostgreSQL healthy. |
| `python3 /tmp/codex-booking-review/compose_check.py` | **PASS**: `down/up` с прежним томом сохраняет данные и ключ; незавершённое тело прервано за `15.02s`; при остановленной БД `/healthz=200`, `/readyz=503`, бизнес-запрос `500` без внутренних подробностей; восстановление после старта БД успешно. Полный текст ниже. |
| `docker compose -p codex-review-compose-20261106 ps`; `docker inspect --format '{{.Config.User}}' codex-review-compose-20261106-app-1` | Проверены фактическое состояние, localhost-привязки портов и `nonroot:nonroot`. |
| Чтение собственных `server-*.log` через `rg` | Подтверждены SQLSTATE 22021 для NUL, однократное применение миграции, штатное завершение процессов. |
| `APP_PORT=127.0.0.1:38080 POSTGRES_PORT=127.0.0.1:35432 docker compose -p codex-review-compose-20261106 down -v`; `docker rm -fv codex-review-booking-20261106`; `docker image rm codex-review-booking:20261106 codex-review-compose-20261106-app` | **PASS**: собственные тестовые контейнеры, тома, сеть и два созданных образа удалены. Файлы проверок и кеши оставлены в `/tmp`. |

Собственная проверка A дополнительно сравнивала `reserved_seats` с SQL-суммой мест
в подтверждённых бронях, а не только с HTTP-ответом. 100 запросов через два
процесса дали ровно 10 созданий и 90 отказов на 10 мест; 40 отмен освободили места
однократно; устаревшая отмена не затронула новую бронь. После полного завершения
обоих процессов новый процесс увидел прежние записи, счётчик и ключ. Штатные
тесты отдельно подтвердили многоместное бронирование, конфликт payload для ключа,
конкурентную отмену одной брони и повторное применение миграций.

### Что не выполнялось и почему

- `govulncheck`, CVE-сканер образов и иной аудит известных уязвимостей зависимостей
  **не запускались**. Выводов об отсутствии известных уязвимостей нет.
- Не проводились тяжёлые нагрузочные атаки, заполнение БД/диска, массовое
  исчерпание памяти и соединений. Для безопасности и конкурентности использованы
  ограниченные сценарии только на собственных сервисах.
- Не моделировались падение ОС/питания, failover кластера PostgreSQL, сетевое
  разделение во время `COMMIT`, восстановление из backup и потеря/повреждение
  тома. Проверены штатные перезапуски процессов, БД и пересоздание контейнеров.
- Публичная сеть, настоящий reverse proxy, TLS и разграничение прав не
  проверялись: они не входят в предоставленное решение и не требуются TASK.md.
- Makefile-алиасы отдельно не запускались: выполнены соответствующие команды
  Go напрямую. Не выполнялись изменяющие код `make fmt` и исправления реализации.

## Сильные стороны

- Существенные гарантии опираются на PostgreSQL и сохраняются между экземплярами.
- Изменение счётчика и брони атомарно; повторная отмена защищена блокировкой строки.
- Ключ и hash запроса хранятся в БД; уникальный индекс исключает дубликаты,
  конфликт изменённого тела не замалчивается.
- Подготовка схемы встроена в бинарник, транзакционна и защищена от одновременного
  применения; воспроизводимость и сохранность тома подтверждены запуском.
- Тесты проверяют настоящую БД и существенные конкурентные сценарии;
  ошибки не скрываются пропуском интеграционных тестов при отсутствии Docker.
- README подробно описывает API, гарантии, команды и реальные ограничения,
  включая локальные dev-реквизиты и отсутствие пагинации/авторизации.

## Приложение A. Полный текст собственной проверки

Сохранить следующий код как `/tmp/codex-booking-review/check.py`. Запускать на
**собственной пустой** БД `review` в контейнере с указанным выше именем и ролью
`review`, после сборки `/tmp/codex-booking-review/server`. Скрипт намеренно
оставляет тестовые данные в этой временной БД; PostgreSQL-команды не направлять
в рабочую БД. Порт заменить на фактически выданный `docker port`, если он другой.

```bash
TEST_DATABASE_URL='postgres://review:review@127.0.0.1:32768/review?sslmode=disable' \
  python3 /tmp/codex-booking-review/check.py
```

```python
import concurrent.futures as cf
import json
import os
import pathlib
import socket
import subprocess as sp
import time
import urllib.error
import urllib.request

ROOT = pathlib.Path('/tmp/codex-booking-review')
PG = os.getenv('REVIEW_PG', 'codex-review-booking-20261106')
DSN = os.environ['TEST_DATABASE_URL']
PSQL = ['docker', 'exec', '-i', PG, 'psql', '-X', '-qAt',
        '-v', 'ON_ERROR_STOP=1', '-U', 'review', '-d', 'review']
processes = []
logs = []


def sql(query):
    return sp.check_output(PSQL + ['-c', query], text=True).strip()


def http(base, method, path, data=None, key=None):
    if data is not None and not isinstance(data, bytes):
        data = json.dumps(data).encode()
    req = urllib.request.Request(base + path, data=data, method=method)
    if data is not None:
        req.add_header('Content-Type', 'application/json')
    if key is not None:
        req.add_header('Idempotency-Key', key)
    try:
        response = urllib.request.urlopen(req, timeout=25)
    except urllib.error.HTTPError as error:
        response = error
    with response:
        return response.status, dict(response.headers), json.loads(response.read())


def start(index):
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0))
        port = sock.getsockname()[1]
    log = open(ROOT / f'server-{index}.log', 'a')
    logs.append(log)
    proc = sp.Popen([str(ROOT / 'server')], stdout=log, stderr=log,
                    env=dict(os.environ, DATABASE_URL=DSN,
                             HTTP_ADDR=f'127.0.0.1:{port}'))
    processes.append(proc)
    return proc, f'http://127.0.0.1:{port}'


def ready(base):
    for _ in range(100):
        try:
            if http(base, 'GET', '/readyz')[0] == 200:
                return
        except OSError:
            pass
        time.sleep(0.1)
    raise AssertionError('server did not become ready')


def event(base, capacity):
    status, _, body = http(base, 'POST', '/events', {
        'name': 'review', 'capacity': capacity,
        'starts_at': '2026-12-01T18:00:00Z'})
    assert status == 201, (status, body)
    return body['id']


def book(base, eid, key, seats=1, **fields):
    return http(base, 'POST', f'/events/{eid}/bookings',
                dict(seats=seats, **fields), key)


def state(base, eid, reserved, count):
    body = http(base, 'GET', f'/events/{eid}')[2]
    assert body['reserved_seats'] == reserved, body
    assert int(sql(f"SELECT count(*) FROM bookings WHERE event_id='{eid}'")) == count
    assert int(sql(f"SELECT COALESCE(sum(seats),0) FROM bookings "
                   f"WHERE event_id='{eid}' AND status='confirmed'")) == reserved


try:
    # Both processes start on a completely empty database.
    p1, a = start(1)
    p2, b = start(2)
    ready(a)
    ready(b)
    assert sql('SELECT count(*) FROM schema_migrations') == '1'
    print('PASS: concurrent process startup and schema preparation', flush=True)

    eid = event(a, 10)
    with cf.ThreadPoolExecutor(max_workers=30) as pool:
        responses = list(pool.map(lambda i: book([a, b][i % 2], eid, f'load-{i}'), range(100)))
    assert sum(r[0] == 201 for r in responses) == 10
    assert sum(r[0] == 409 for r in responses) == 90
    state(b, eid, 10, 10)
    ids = [r[2]['id'] for r in responses if r[0] == 201]
    with cf.ThreadPoolExecutor(max_workers=20) as pool:
        cancellations = list(pool.map(lambda i: http([a, b][i % 2], 'DELETE',
                                '/bookings/' + ids[i % 10]), range(40)))
    assert all(r[0] == 200 for r in cancellations)
    state(a, eid, 0, 10)
    assert book(b, eid, 'after-cancel', 10)[0] == 201
    assert http(a, 'DELETE', '/bookings/' + ids[0])[0] == 200
    state(b, eid, 10, 11)
    print('PASS: 100 bookings and 40 cancellations across two processes; stale cancellation', flush=True)

    # Force both idempotency lookups to finish before either UPDATE can complete.
    eid = event(a, 1)
    lock = sp.Popen(PSQL, stdin=sp.PIPE, stdout=sp.PIPE, stderr=sp.PIPE,
                    text=True, bufsize=1)
    lock.stdin.write(f"BEGIN; SELECT id FROM events WHERE id='{eid}' FOR UPDATE;\n\\echo LOCKED\n")
    lock.stdin.flush()
    while lock.stdout.readline().strip() != 'LOCKED':
        assert lock.poll() is None, 'lock session stopped'
    try:
        with cf.ThreadPoolExecutor(max_workers=2) as pool:
            futures = [pool.submit(book, base, eid, 'same-key') for base in [a, b]]
            deadline = time.monotonic() + 8
            while True:
                waiting = int(sql("SELECT count(*) FROM pg_stat_activity WHERE "
                    "application_name='booking-service' AND wait_event_type='Lock' "
                    "AND position('SET reserved_seats = reserved_seats + $2' in query)>0"))
                if waiting == 2:
                    break
                assert time.monotonic() < deadline, f'only {waiting} blocked requests'
                time.sleep(0.05)
            lock.stdin.write('COMMIT;\n\\q\n')
            lock.stdin.flush()
            responses = [f.result() for f in futures]
    finally:
        if lock.poll() is None:
            lock.stdin.write('ROLLBACK;\n\\q\n')
            lock.stdin.flush()
        lock.wait(timeout=5)
    print('OBSERVE idempotency with last seat: expected [200, 201], got',
          sorted(r[0] for r in responses), responses, flush=True)
    state(a, eid, 1, 1)
    winner = next(r[2]['id'] for r in responses if r[0] == 201)
    assert book(b, eid, 'same-key')[0] == 200
    assert http(a, 'DELETE', '/bookings/' + winner)[0] == 200
    replay = book(b, eid, 'same-key')
    assert replay[0] == 200 and replay[2]['status'] == 'cancelled'
    state(a, eid, 0, 1)
    print('PASS: sequential replay, replay after cancellation, no duplicate rows', flush=True)

    before = sql('SELECT count(*) FROM events')
    result = http(a, 'POST', '/events', {'name': 'bad\x00name', 'capacity': 2,
                  'starts_at': '2026-12-01T18:00:00Z'})
    print('OBSERVE NUL event name: expected 400, got', result, flush=True)
    assert sql('SELECT count(*) FROM events') == before
    eid = event(b, 2)
    result = book(a, eid, 'nul-customer', customer_name='bad\x00name')
    print('OBSERVE NUL customer name: expected 400, got', result, flush=True)
    state(b, eid, 0, 0)

    invalid = [b'{', b'null', b'{}{}', b'{"unknown":true}',
               b'{"name":"x","capacity":1.2,"starts_at":"2026-12-01T18:00:00Z"}',
               b'{"name":"x","capacity":9223372036854775808,"starts_at":"2026-12-01T18:00:00Z"}']
    for raw in invalid:
        assert http(a, 'POST', '/events', raw)[0] == 400
    oversized = b'{"name":"' + b'x' * (1 << 20) + b'"}'
    assert http(a, 'POST', '/events', oversized)[0] == 413
    padded = b'{"name":"x","capacity":1,"starts_at":"2026-12-01T18:00:00Z"}' + b' ' * (1 << 20)
    print('OBSERVE oversized trailing whitespace:', http(a, 'POST', '/events', padded)[0], flush=True)
    attack = "x'); DROP TABLE events; --"
    result = book(b, eid, attack, customer_name=attack)
    assert result[0] == 201 and result[2]['customer_name'] == attack
    state(a, eid, 1, 1)
    print('PASS: malformed/unknown/multiple/overflow JSON; size limit; SQL payload stored as data', flush=True)

    # Full process restart applies migrations again and preserves bookings and keys.
    for proc in [p1, p2]:
        proc.terminate()
        assert proc.wait(timeout=15) == 0
    p3, c = start(3)
    ready(c)
    state(c, eid, 1, 1)
    assert book(c, eid, attack, customer_name=attack)[0] == 200
    assert sql('SELECT count(*) FROM schema_migrations') == '1'
    print('PASS: actual process restart preserves data, counter and idempotency key', flush=True)
finally:
    for proc in processes:
        if proc.poll() is None:
            proc.terminate()
            proc.wait(timeout=15)
    for log in logs:
        log.close()

```

## Приложение B. Полный текст проверки Compose

Сохранить код как `/tmp/codex-booking-review/compose_check.py`. Запускать из корня проекта после приведённой выше команды `compose up` для собственного проекта `codex-review-compose-20261106`. Скрипт останавливает и пересоздаёт только этот временный Compose-проект, сохраняя его том.

```bash
python3 /tmp/codex-booking-review/compose_check.py
```

```python
import json
import os
import socket
import subprocess as sp
import time
import urllib.error
import urllib.request

BASE = 'http://127.0.0.1:38080'
CMD = ['docker', 'compose', '-p', 'codex-review-compose-20261106']
ENV = dict(os.environ, APP_PORT='127.0.0.1:38080', POSTGRES_PORT='127.0.0.1:35432')


def compose(*args):
    sp.run(CMD + list(args), env=ENV, check=True)


def request(method, path, body=None, key=None):
    raw = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(BASE + path, data=raw, method=method)
    if raw is not None:
        req.add_header('Content-Type', 'application/json')
    if key:
        req.add_header('Idempotency-Key', key)
    try:
        response = urllib.request.urlopen(req, timeout=20)
    except urllib.error.HTTPError as error:
        response = error
    with response:
        return response.status, json.loads(response.read())


def ready():
    deadline = time.monotonic() + 40
    while time.monotonic() < deadline:
        try:
            if request('GET', '/readyz')[0] == 200:
                return
        except OSError:
            pass
        time.sleep(0.2)
    raise AssertionError('compose service did not become ready')


ready()
status, event = request('POST', '/events', {'name': 'compose persistence',
                        'capacity': 3, 'starts_at': '2026-12-01T18:00:00Z'})
assert status == 201, event
path = '/events/' + event['id'] + '/bookings'
status, booking = request('POST', path, {'seats': 2}, 'compose-key')
assert status == 201, booking
compose('down')
compose('up', '-d')
ready()
status, replay = request('POST', path, {'seats': 2}, 'compose-key')
assert status == 200 and replay['id'] == booking['id'], (status, replay)
status, result = request('GET', '/events/' + event['id'])
assert status == 200 and result['reserved_seats'] == 2, result
print('PASS: compose down/up preserves event, booking, counter and key in named volume', flush=True)

# One slow request, without any load test.
with socket.create_connection(('127.0.0.1', 38080), timeout=20) as conn:
    start = time.monotonic()
    conn.sendall(b'POST /events HTTP/1.1\r\nHost: localhost\r\n'
                 b'Content-Type: application/json\r\nContent-Length: 100\r\n\r\n{')
    response = conn.recv(4096)
    elapsed = time.monotonic() - start
    assert response.startswith(b'HTTP/1.1 400'), response
    assert elapsed < 20, elapsed
    print('PASS: unfinished body stopped after', round(elapsed, 2), 'seconds', flush=True)

compose('stop', 'postgres')
try:
    assert request('GET', '/healthz')[0] == 200
    assert request('GET', '/readyz')[0] == 503
    status, error = request('GET', '/events')
    assert status == 500 and error['error']['code'] == 'internal_error', (status, error)
    print('PASS: unavailable DB: health=200, readiness=503, generic business error=500', flush=True)
finally:
    compose('start', 'postgres')
ready()
assert request('GET', '/events/' + event['id'])[1]['reserved_seats'] == 2
print('PASS: recovery after own PostgreSQL restart', flush=True)

```
