package admin

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/srgsf/adsvc/internal/catalog"
	"github.com/srgsf/adsvc/internal/record"
)

type originView struct {
	Origin       record.Origin `json:"origin"`
	Name         string        `json:"name"`
	Weight       float64       `json:"weight"`
	Own          *float64      `json:"own_weight,omitempty"`
	Blocked      bool          `json:"blocked"`
	Managed      bool          `json:"managed"`
	Self         bool          `json:"self"`
	Records      int           `json:"records"`
	LastReceived time.Time     `json:"last_received,omitzero"`
	Ads          int           `json:"ads"`
	Trashed      int           `json:"trashed"`
	Dups         int           `json:"dups"`
	Retracted    int           `json:"retracted"`
	Votes        int           `json:"votes"`
}

type originsPage struct {
	Rows     []originView `json:"origins"`
	Unknown  float64      `json:"unknown_weight"`
	MinTrust float64      `json:"min_trust"`
}

func (a *Admin) loadOrigins(r *http.Request, p *page) error {
	rows, err := a.d.DB.Origins(r.Context())
	if err != nil {
		return err
	}
	pol := a.d.DB.Policy()
	d := originsPage{Unknown: pol.UnknownWeight, MinTrust: pol.MinTrust}
	for _, o := range rows {
		v := originView{Origin: o.Origin, Name: o.Name, Weight: o.Weight, Own: o.Own, Blocked: o.Blocked, Managed: o.Managed,
			Self: o.Self, Records: o.Records, Ads: o.Ads, Trashed: o.Trashed, Dups: o.Dups, Retracted: o.Retracted, Votes: o.Votes}
		v.LastReceived = o.LastReceived
		d.Rows = append(d.Rows, v)
	}
	p.Data = d
	return nil
}

func (a *Admin) origins(w http.ResponseWriter, r *http.Request) {
	p := a.newPage(r, "origins")
	if err := a.loadOrigins(r, p); err != nil {
		a.internal(w, r, err)
		return
	}
	a.show(w, r, p, "origins-table", http.StatusOK)
}

// originSet sets this node's opinion of an origin (name, weight, blocked), or forgets it
// (action=clear). Origins from the config file are read-only here.
func (a *Admin) originSet(w http.ResponseWriter, r *http.Request) {
	p := a.newPage(r, "origins")
	o, err := record.ParseOrigin(r.PathValue("origin"))
	if err != nil {
		a.fail(w, r, http.StatusBadRequest, err)
		return
	}
	if r.PostFormValue("action") == "clear" {
		err = a.d.DB.ClearOriginPolicy(a.ctx, o)
	} else {
		rule := catalog.OriginRule{Origin: o, Name: strings.TrimSpace(r.PostFormValue("name")), Blocked: r.PostFormValue("blocked") != ""}
		if s := strings.TrimSpace(r.PostFormValue("weight")); s != "" {
			weight, perr := strconv.ParseFloat(s, 64)
			if perr != nil || weight < 0 {
				a.fail(w, r, http.StatusBadRequest, errors.New("weight: want a number ≥ 0, or nothing for the default"))
				return
			}
			rule.Weight = &weight
		}
		err = a.d.DB.SetOriginPolicy(a.ctx, rule)
	}
	switch {
	case errors.Is(err, catalog.ErrManaged), errors.Is(err, catalog.ErrSelf):
		a.fail(w, r, http.StatusConflict, err)
		return
	case err != nil:
		a.fail(w, r, http.StatusUnprocessableEntity, err)
		return
	}
	if err := a.refresh(); err != nil {
		a.internal(w, r, err)
		return
	}
	p.Msg = p.T("origins.saved")
	if err := a.loadOrigins(r, p); err != nil {
		a.internal(w, r, err)
		return
	}
	a.show(w, r, p, "origins-table", http.StatusOK)
}
