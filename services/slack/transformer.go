package slack

import log "github.com/sirupsen/logrus"

type Transformer struct {
	TeamName     string
	Intermediate *Intermediate
	Logger       log.FieldLogger
}

func NewTransformer(teamName string, logger log.FieldLogger) *Transformer {
	return &Transformer{
		TeamName:     teamName,
		Intermediate: &Intermediate{},
		Logger:       logger,
	}
}

// CloneForWorker creates a Transformer suitable for parallel post processing.
// Channel slices are shared (read-only during post processing).
// UsersById is shallow-copied so each worker can independently create
// placeholder users/bots without races.
func (t *Transformer) CloneForWorker() *Transformer {
	usersById := make(map[string]*IntermediateUser, len(t.Intermediate.UsersById))
	for k, v := range t.Intermediate.UsersById {
		usersById[k] = v
	}
	return &Transformer{
		TeamName: t.TeamName,
		Intermediate: &Intermediate{
			PublicChannels:  t.Intermediate.PublicChannels,
			PrivateChannels: t.Intermediate.PrivateChannels,
			GroupChannels:   t.Intermediate.GroupChannels,
			DirectChannels:  t.Intermediate.DirectChannels,
			UsersById:       usersById,
		},
		Logger: t.Logger,
	}
}
