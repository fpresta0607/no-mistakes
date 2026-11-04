package config

// PublishesIntent requires an explicit opt-in to use validation intent in
// public PR sections and drafting prompts. Validation prompts are unaffected.
func (p PR) PublishesIntent() bool {
	return p.PublishIntent != nil && *p.PublishIntent
}
