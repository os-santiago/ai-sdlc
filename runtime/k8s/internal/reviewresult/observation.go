package reviewresult


const ObservationVersion = "ai-sdlc.review-observation/v1"

// Observation is derived from a provider response before delivery to the worker.
// It contains no source, arguments or model prose and grants no authority.
type Observation struct {
	Version     string         `json:"version"`
	Tools       map[string]int `json:"tools"`
	Verdict     string         `json:"verdict"`
	FinalSHA256 string         `json:"finalSHA256"`
}

// MatchObservations binds an untrusted summary to ordered provider observations.
// The caller must authenticate the journal and its sequence/response identities.
func MatchObservations(summary Result, evidence []Observation) error {
	if len(evidence) < 2 || len(evidence) > 8 || summary.Iterations != len(evidence) {
		return ErrInvalid
	}
	total := 0
	for i, o := range evidence {
		if o.Version != ObservationVersion || o.Tools == nil {
			return ErrInvalid
		}
		if i == len(evidence)-1 {
			if len(o.Tools) != 0 || o.Verdict != summary.Verdict || o.FinalSHA256 != summary.BoundFinalSHA256() {
				return ErrInvalid
			}
		} else {
			if o.Verdict != "" || o.FinalSHA256 != "" || len(o.Tools) == 0 {
				return ErrInvalid
			}
			count := 0
			for name, n := range o.Tools {
				if name != "read_file" && name != "list_dir" && name != "search_text" && name != "code_query" {
					return ErrInvalid
				}
				if n < 1 || n > 4 {
					return ErrInvalid
				}
				count += n
			}
			if count > 4 {
				return ErrInvalid
			}
			total += count
		}
	}
	verdict, err := Verdict(summary.FinalMessage)
	if err != nil || verdict != summary.Verdict || total != summary.ReadCalls {
		return ErrInvalid
	}
	return nil
}
