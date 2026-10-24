package main

import "strings"

// Claude Code's AskUserQuestion tool puts a multiple-choice question in front
// of the candidate and hands Claude their answer. The generic tool_use branch
// kept a 100-char JSON preview of it, so a reviewer saw neither the question,
// the options, nor what the candidate chose — the answer survived only inside
// Claude's next request, phrased as prose. The event below carries the whole
// exchange.
//
// Verified shapes (Claude Code 2.1.x). Hook PostToolUse sends tool_input with
// the answers merged in and tool_response with the same object; the transcript
// carries the same object as toolUseResult:
//
//	{"questions":[{"question","header","multiSelect","options":[{"label","description","preview?"}]}],
//	 "answers":{"<question>":"<label>" | "<label>, <label>" | "<free text>"},
//	 "annotations":{"<question>":{"notes?","preview?"}}}
//
// A dismissed question arrives as an error result with no answers.

// Caps keep one event well under the ingest body limit while leaving every
// realistic question, option and answer whole.
const (
	askQuestionMaxChars = 1000
	askAnswerMaxChars   = 4000
)

// normalizeAskUserQuestion builds the question_answer event. Questions are read
// from the response first (it is what the candidate actually saw) and fall back
// to the input; the same for answers and annotations.
func normalizeAskUserQuestion(toolInput, toolResponse map[string]interface{}, sessionID, raw string) Event {
	pick := func(key string) interface{} {
		if v, ok := toolResponse[key]; ok && v != nil {
			return v
		}
		return toolInput[key]
	}
	rawQuestions, _ := pick("questions").([]interface{})
	answers, _ := pick("answers").(map[string]interface{})
	annotations, _ := pick("annotations").(map[string]interface{})

	questions := make([]map[string]interface{}, 0, len(rawQuestions))
	answered := false
	for _, rq := range rawQuestions {
		q, _ := rq.(map[string]interface{})
		if q == nil {
			continue
		}
		text := stringField(q, "question")
		multi, _ := q["multiSelect"].(bool)

		var labels []string
		options := []map[string]interface{}{}
		rawOptions, _ := q["options"].([]interface{})
		for _, ro := range rawOptions {
			o, _ := ro.(map[string]interface{})
			if o == nil {
				continue
			}
			label := stringField(o, "label")
			labels = append(labels, label)
			opt := map[string]interface{}{"label": strPreview(label, askQuestionMaxChars)}
			if d := stringField(o, "description"); d != "" {
				opt["description"] = strPreview(d, askQuestionMaxChars)
			}
			options = append(options, opt)
		}

		entry := map[string]interface{}{
			"question":    strPreview(text, askQuestionMaxChars),
			"multiSelect": multi,
			"options":     options,
			"selected":    []string{},
		}
		if h := stringField(q, "header"); h != "" {
			entry["header"] = h
		}
		if answer, _ := answers[text].(string); answer != "" {
			answered = true
			selected, other := splitQuestionAnswer(answer, labels, multi)
			entry["answer"] = strPreview(answer, askAnswerMaxChars)
			entry["selected"] = selected
			if other != "" {
				entry["otherText"] = strPreview(other, askAnswerMaxChars)
			}
		}
		if ann, ok := annotations[text].(map[string]interface{}); ok {
			if notes := stringField(ann, "notes"); notes != "" {
				entry["notes"] = strPreview(notes, askAnswerMaxChars)
			}
		}
		questions = append(questions, entry)
	}

	status := "declined"
	if answered {
		status = "answered"
	}
	e := newEvent("question_answer", sessionID)
	// The answer is the candidate's call; the question is Claude's, and the
	// paired tool_intent already records Claude asking it.
	e.Actor = humanActor()
	e.Provenance = humanProvenance()
	e.Data = map[string]interface{}{
		"toolName":  "AskUserQuestion",
		"status":    status,
		"questions": questions,
	}
	e.RawPayload = raw
	return e
}

// splitQuestionAnswer separates an answer string into the option labels it
// names and any free text the candidate typed into "Other". A multi-select
// answer is the chosen labels joined with ", " — labels may themselves contain
// ", ", so at each position the longest run of parts that spells a label wins.
func splitQuestionAnswer(answer string, labels []string, multi bool) ([]string, string) {
	isLabel := map[string]bool{}
	for _, l := range labels {
		if l != "" {
			isLabel[l] = true
		}
	}
	if isLabel[answer] {
		return []string{answer}, ""
	}
	if !multi {
		return []string{}, answer
	}
	parts := strings.Split(answer, ", ")
	selected := []string{}
	var other []string
	for i := 0; i < len(parts); {
		matched := 0
		for j := len(parts); j > i; j-- {
			if isLabel[strings.Join(parts[i:j], ", ")] {
				matched = j - i
				break
			}
		}
		if matched == 0 {
			other = append(other, parts[i])
			i++
			continue
		}
		selected = append(selected, strings.Join(parts[i:i+matched], ", "))
		i += matched
	}
	return selected, strings.Join(other, ", ")
}
