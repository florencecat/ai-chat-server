package advice

import "encoding/json"

// schemaJSON — схема ответа, которую сервер передаёт модели
// (response_format.json_schema у Yandex AI Studio).
//
// Намеренно без $ref/$defs: провайдер разбирает схему сам, и чем она проще,
// тем меньше шансов, что он отвергнет запрос. Ограничения вроде maxItems
// продублированы в normalize() — схема это просьба, нормализация это гарантия.
const schemaJSON = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["response"],
  "properties": {
    "response": {
      "type": "string",
      "description": "Ответ пользователю."
    },
    "events": {
      "type": "array",
      "maxItems": 3,
      "description": "Предложенные напоминания. Пустой массив, если повода нет.",
      "items": {
        "type": "object",
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
      "maxItems": 5,
      "description": "Предложенные записи в трекеры. Пустой массив, если записывать нечего.",
      "items": {
        "type": "object",
        "required": ["tracker"],
        "properties": {
          "tracker":   {"type": "string", "enum": ["mood", "weight", "symptom", "walk", "meal", "note"]},
          "datetime":  {"type": "string", "description": "Местное время пользователя, YYYY-MM-DDTHH:MM:SS."},
          "mood":      {"type": "string", "enum": ["happy", "calm", "sick", "playful"], "description": "Только для tracker=mood, обязательно."},
          "weight_kg": {"type": "number", "description": "Только для tracker=weight, обязательно, больше нуля."},
          "symptom":   {"type": "string", "enum": ["vomiting", "diarrhea", "refused_food", "lethargy", "sneezing", "coughing", "scratching", "limping"], "description": "Только для tracker=symptom, обязательно."},
          "severity":  {"type": "string", "enum": ["mild", "moderate", "severe"], "description": "Только для tracker=symptom."},
          "note":      {"type": "string", "description": "Только для tracker=symptom."},
          "minutes":   {"type": "integer", "description": "Только для tracker=walk, обязательно, больше нуля."},
          "activities": {
            "type": "array",
            "items": {"type": "string", "enum": ["active", "calm", "dogGames", "training"]},
            "description": "Только для tracker=walk."
          },
          "grams":    {"type": "number", "description": "Только для tracker=meal; обязателен grams или food."},
          "food":     {"type": "string", "description": "Только для tracker=meal; обязателен grams или food."},
          "appetite": {"type": "integer", "description": "Только для tracker=meal, от 1 до 5."},
          "kind":     {"type": "string", "enum": ["natural", "dry", "wet", "treat"], "description": "Только для tracker=meal."},
          "text":     {"type": "string", "description": "Только для tracker=note, обязательно."}
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
      "maxItems": 5,
      "items": {"type": "string"},
      "description": "Тревожные признаки, при которых нужно к врачу немедленно."
    },
    "follow_up_questions": {
      "type": "array",
      "maxItems": 3,
      "items": {"type": "string"},
      "description": "Уточняющие вопросы пользователю."
    }
  }
}`

// Schema возвращает схему ответа для провайдера.
func Schema() json.RawMessage { return json.RawMessage(schemaJSON) }
