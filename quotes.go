package main

import (
	"math/rand/v2"
	"net/http"
	"sync"
)

// quote is one short line of French rap with its attribution. Lines are kept to a hook or two at most.
type quote struct {
	Text   string `json:"text"`
	Artist string `json:"artist"`
	Track  string `json:"track"`
}

var quotes = []quote{
	{"Demain, c'est loin.", "IAM", "Demain c'est loin"},
	{"Je danse le mia.", "IAM", "Je danse le Mia"},
	{"Petit frère, ne te trompe pas de chemin.", "IAM", "Petit frère"},
	{"Qu'est-ce qu'on attend pour foutre le feu ?", "Suprême NTM", "Qu'est-ce qu'on attend pour foutre le feu"},
	{"Pose ton gun.", "Suprême NTM", "Pose ton gun"},
	{"Bouge de là.", "MC Solaar", "Bouge de là"},
	{"Victime de la mode.", "MC Solaar", "Victime de la mode"},
	{"Je zappe et je mate.", "Passi", "Je zappe et je mate"},
	{"Le monde ou rien.", "PNL", "Le monde ou rien"},
	{"Dans la légende.", "PNL", "Dans la légende"},
	{"On m'appelle l'ovni.", "Jul", "L'ovni"},
	{"Sapés comme jamais.", "Maître Gims ft. Niska", "Sapés comme jamais"},
	{"Désolé, désolé, je n'ai pas le temps.", "Sexion d'Assaut", "Désolé"},
	{"Dommage.", "Bigflo & Oli", "Dommage"},
	{"Le combat continue.", "Kery James", "Le combat continue"},
	{"Banlieusards.", "Kery James", "Banlieusards"},
	{"Tricheur.", "Nekfeu", "Tricheur"},
	{"On verra.", "Nekfeu", "On verra"},
	{"Si seulement je pouvais lui manquer.", "Orelsan", "Si seulement je pouvais lui manquer"},
	{"La terre est ronde.", "Orelsan", "La terre est ronde"},
	{"Basique.", "Orelsan", "Basique"},
	{"Ouest side.", "Booba", "Ouest Side"},
	{"92i Veyron.", "Booba", "92i Veyron"},
	{"Validé.", "Booba", "Validé"},
	{"Mama Lova.", "Oxmo Puccino", "Mama Lova"},
	{"L'enfant seul.", "Oxmo Puccino", "L'enfant seul"},
	{"La boulette.", "Diam's", "La boulette"},
	{"Cosmopolitanie.", "Soprano", "Cosmopolitanie"},
	{"Mon Everest.", "Soprano", "Mon Everest"},
	{"Respect.", "Alliance Ethnik", "Respect"},
	{"Champs-Élysées.", "SCH", "Champs-Élysées"},
	{"Trop beau.", "Lomepal", "Trop beau"},
	{"Goutte d'eau.", "Ninho", "Goutte d'eau"},
	{"Réseaux.", "Niska", "Réseaux"},
}

var (
	quoteMu   sync.Mutex
	lastQuote = -1
)

// nextQuote picks a random quote, never the same index twice in a row when there is a choice.
func nextQuote() quote {
	quoteMu.Lock()
	defer quoteMu.Unlock()
	i := rand.IntN(len(quotes))
	if len(quotes) > 1 {
		for i == lastQuote {
			i = rand.IntN(len(quotes))
		}
	}
	lastQuote = i
	return quotes[i]
}

func handleQuote(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, nextQuote())
}
