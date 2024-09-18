package bloom

type Optons struct {
	store Store
}

type option func(*Optons)

func WithStore(s Store) option {
	return func(o *Optons) {
		o.store = s
	}
}
