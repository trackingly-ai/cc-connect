package core

func (e *Engine) AgentName() string {
	if e == nil || e.agent == nil {
		return ""
	}
	return e.agent.Name()
}
