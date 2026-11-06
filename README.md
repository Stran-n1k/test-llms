# Сравнение Go coding agents: сервис бронирования

Пять агентов выполнили одинаковое задание на Go и PostgreSQL. Затем каждое
решение независимо проверили четыре другие модели. Сохранены пять исходных
реализаций и все 20 отзывов. Обоснованность всех замечаний ещё не проверена
независимо.

**Итоговые рейтинги качества разработки и ревью, критерии и ограничения оценки — в [RESULTS.md](RESULTS.md).**

| Участник | Клиент | Модель (High) | Время реализации по интерфейсу | Ветка |
|---|---|---|---|---|
| claude | Claude Code 2.1.318 | Opus 5.5 | 44:54 | [solution/claude](https://github.com/Stran-n1k/test-llms/tree/solution/claude) |
| codex | Codex CLI 0.166.0 | GPT-6.1-Sol | 12:00 | [solution/codex](https://github.com/Stran-n1k/test-llms/tree/solution/codex) |
| deepseek | OpenCode 2.2.5 | DeepSeek V4.1 Flash 1017 | 22:26 | [solution/deepseek](https://github.com/Stran-n1k/test-llms/tree/solution/deepseek) |
| glm | OpenCode 2.2.5 | GLM-5.3 | 26:59 | [solution/glm](https://github.com/Stran-n1k/test-llms/tree/solution/glm) |
| kimi | OpenCode 2.2.5 | Kimi K3.1 Pro | 15:05 | [solution/kimi](https://github.com/Stran-n1k/test-llms/tree/solution/kimi) |

Время отражает один прогон и включает возможные ожидания разрешений и API.
Часть прогонов шла параллельно. У Kimi наблюдались повторные запросы из-за
лимита 3 RPM. Эти значения нельзя считать чистым измерением скорости модели.

## Как посмотреть решение

Выберите ветку `solution/<участник>` в интерфейсе GitHub или переключитесь локально:

```bash
git switch solution/claude
```

В корне каждой ветки находятся исходный `TASK.md`, код, README автора,
Dockerfile, Compose и четыре файла `<проверяющий>_review.md`.
Первый коммит ветки фиксирует реализацию, следующий добавляет отзывы.
Исправления по результатам отзывов в исходные решения не вносились при сборке.
Общее описание эксперимента сохранено в `benchmark/EXPERIMENT.md` во всех ветках.

## Что где находится

- [TASK.md](TASK.md) — общий контракт задачи.
- [Описание эксперимента](benchmark/EXPERIMENT.md) — условия, процедура и ограничения.
- [Инструкции проверяющим](benchmark/review-prompts/) — полные тексты задания на ревью.
- [Манифест](benchmark/manifest.json) — состав, контрольные суммы и сведения о моделях.

Все отзывы собраны без редактирования. Формулировка «подтверждено» в отзыве
означает заявление его автора, а не независимую верификацию организатором.
Отсутствие замечаний не доказывает отсутствие дефектов.

## Матрица ревью

Строка — проверяющий, столбец — автор решения. Ссылки ведут на полные отзывы.

| Проверяющий \ Автор | claude | codex | deepseek | glm | kimi |
|---|---|---|---|---|---|
| claude | — | [Ревью](https://github.com/Stran-n1k/test-llms/blob/solution/codex/claude_review.md) | [Ревью](https://github.com/Stran-n1k/test-llms/blob/solution/deepseek/claude_review.md) | [Ревью](https://github.com/Stran-n1k/test-llms/blob/solution/glm/claude_review.md) | [Ревью](https://github.com/Stran-n1k/test-llms/blob/solution/kimi/claude_review.md) |
| codex | [Ревью](https://github.com/Stran-n1k/test-llms/blob/solution/claude/codex_review.md) | — | [Ревью](https://github.com/Stran-n1k/test-llms/blob/solution/deepseek/codex_review.md) | [Ревью](https://github.com/Stran-n1k/test-llms/blob/solution/glm/codex_review.md) | [Ревью](https://github.com/Stran-n1k/test-llms/blob/solution/kimi/codex_review.md) |
| deepseek | [Ревью](https://github.com/Stran-n1k/test-llms/blob/solution/claude/deepseek_review.md) | [Ревью](https://github.com/Stran-n1k/test-llms/blob/solution/codex/deepseek_review.md) | — | [Ревью](https://github.com/Stran-n1k/test-llms/blob/solution/glm/deepseek_review.md) | [Ревью](https://github.com/Stran-n1k/test-llms/blob/solution/kimi/deepseek_review.md) |
| glm | [Ревью](https://github.com/Stran-n1k/test-llms/blob/solution/claude/glm_review.md) | [Ревью](https://github.com/Stran-n1k/test-llms/blob/solution/codex/glm_review.md) | [Ревью](https://github.com/Stran-n1k/test-llms/blob/solution/deepseek/glm_review.md) | — | [Ревью](https://github.com/Stran-n1k/test-llms/blob/solution/kimi/glm_review.md) |
| kimi | [Ревью](https://github.com/Stran-n1k/test-llms/blob/solution/claude/kimi_review.md) | [Ревью](https://github.com/Stran-n1k/test-llms/blob/solution/codex/kimi_review.md) | [Ревью](https://github.com/Stran-n1k/test-llms/blob/solution/deepseek/kimi_review.md) | [Ревью](https://github.com/Stran-n1k/test-llms/blob/solution/glm/kimi_review.md) | — |
