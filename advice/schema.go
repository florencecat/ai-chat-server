package advice

import "encoding/json"

// schemaJSON — схема ответа, которую сервер передаёт модели
// (response_format.json_schema у Yandex AI Studio).
//
// Схема строгая в смысле OpenAI strict mode: Yandex отвергает её целиком,
// если хоть одно свойство не перечислено в required («Invalid JSON Schema:
// all fields must be required, 'events' is optional»). Отсюда два следствия.
//
// Первое: модель обязана прислать все поля верхнего уровня. Пустые events,
// entries и red_flags приходят как [], а urgency — всегда, поэтому различать
// «метаданных нет» по его отсутствию больше нельзя.
//
// Второе: поля entries принадлежат разным трекерам и осмысленны только для
// своего, но прислать модель обязана все. Поэтому в перечисления добавлена
// пустая строка — легальный способ сказать «не мой трекер», — а чужие поля
// всё равно вычищаются в Entry.normalize перед отдачей клиенту.
//
// Намеренно без $ref/$defs и без maxItems: валидатор Yandex скопирован с
// strict mode OpenAI, где maxItems в число поддерживаемых ключевых слов не
// входит, а чем схема проще, тем меньше шансов, что её отвергнут целиком.
// Количественные лимиты живут в описаниях, в промте и — как гарантия — в
// normalize(). Схема это просьба, нормализация это гарантия.
const schemaJSON = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["response", "events", "entries", "urgency", "red_flags", "follow_up_questions"],
  "properties": {
    "response": {
      "type": "string",
      "description": "Ответ пользователю."
    },
    "events": {
      "type": "array",
      "description": "Предложенные напоминания, не больше трёх. Пустой массив, если повода нет.",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["name", "category", "datetime", "repeat"],
        "properties": {
          "name":     {"type": "string", "description": "Краткое название напоминания."},
          "category": {"type": "string", "enum": ["health", "grooming", "other"]},
          "datetime": {"type": "string", "description": "Местное время пользователя, YYYY-MM-DDTHH:MM:SS, без Z и без смещения."},
          "repeat":   {"type": "string", "enum": ["none", "daily", "weekly", "monthly"]}
        }
      }
    },
    "entries": {
      "type": "array",
      "description": "Предложенные записи в трекеры, не больше пяти. Пустой массив, если записывать нечего. Заполняй только поля своего трекера; в остальных ставь пустую строку, ноль или пустой список.",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["tracker", "datetime", "mood", "weight_kg", "symptom", "severity", "note", "minutes", "activities", "grams", "food", "appetite", "kind", "text"],
        "properties": {
          "tracker":   {"type": "string", "enum": ["mood", "weight", "symptom", "walk", "meal", "note"]},
          "datetime":  {"type": "string", "description": "Местное время пользователя, YYYY-MM-DDTHH:MM:SS."},
          "mood":      {"type": "string", "enum": ["happy", "calm", "sick", "playful", ""], "description": "Только для tracker=mood, там обязательно. Иначе пустая строка."},
          "weight_kg": {"type": "number", "description": "Только для tracker=weight, там обязательно и больше нуля. Иначе 0."},
          "symptom":   {"type": "string", "enum": ["vomiting", "diarrhea", "refused_food", "lethargy", "sneezing", "coughing", "scratching", "limping", ""], "description": "Только для tracker=symptom, там обязательно. Иначе пустая строка."},
          "severity":  {"type": "string", "enum": ["mild", "moderate", "severe", ""], "description": "Только для tracker=symptom. Иначе пустая строка."},
          "note":      {"type": "string", "description": "Только для tracker=symptom. Иначе пустая строка."},
          "minutes":   {"type": "integer", "description": "Только для tracker=walk, там обязательно и больше нуля. Иначе 0."},
          "activities": {
            "type": "array",
            "items": {"type": "string", "enum": ["active", "calm", "dogGames", "training"]},
            "description": "Только для tracker=walk. Иначе пустой список."
          },
          "grams":    {"type": "number", "description": "Только для tracker=meal; там обязателен grams или food. Иначе 0."},
          "food":     {"type": "string", "description": "Только для tracker=meal; там обязателен grams или food. Иначе пустая строка."},
          "appetite": {"type": "integer", "description": "Только для tracker=meal, от 1 до 5. Иначе 0."},
          "kind":     {"type": "string", "enum": ["natural", "dry", "wet", "treat", ""], "description": "Только для tracker=meal. Иначе пустая строка."},
          "text":     {"type": "string", "description": "Только для tracker=note, там обязательно. Иначе пустая строка."}
        }
      }
    },
    "urgency": {
      "type": "string",
      "enum": ["self_care", "monitor", "vet_24h", "emergency"],
      "description": "Насколько срочно нужна помощь ветеринара."
    },
    "red_flags": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Тревожные признаки, при которых нужно к врачу немедленно, не больше пяти. Пустой список, если их нет."
    },
    "follow_up_questions": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Уточняющие вопросы пользователю, не больше трёх. Пустой список, если вопросов нет."
    }
  }
}`

// Schema возвращает схему ответа для провайдера.
func Schema() json.RawMessage { return json.RawMessage(schemaJSON) }
