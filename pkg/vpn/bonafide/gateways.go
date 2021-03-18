package bonafide

import (
	"errors"
	"log"
	"math/rand"
	"sort"
	"strconv"
	"time"
)

const (
	maxGateways = 3
)

// Load reflects the fullness metric that menshen returns, if available.
type Load struct {
	Host     string
	Fullness string
}

// A Gateway is a representation of gateways that is independent of the api version.
// If a given physical location offers different transports, they will appear
// as separate gateways, so make sure to filter them.
// TODO We should include also the fullness metric here, so that it's easier to the UI to
// represent them without any extra call.
type Gateway struct {
	Host         string
	IPAddress    string
	Location     string
	LocationName string
	CountryCode  string
	Ports        []string
	Protocols    []string
	Options      map[string]string
	Transport    string
	Fullness     float32
}

/* gatewayDistance is used in the timezone distance fallback */
type gatewayDistance struct {
	gateway  Gateway
	distance int
}

type gatewayPool struct {
	/* available is the unordered list of gateways from eip-service, we use if as source-of-truth for now.
	TODO we might want to remove gateways if they are not returned by menshen due to being overloaded */
	available  []Gateway
	userChoice []byte

	/* byCity is a map from location to an array of hostnames */
	byCity map[string][]string

	/* recommended is an array of hostnames, fetched from the old geoip service.
	 *  this should be deprecated in favor of recommendedWithLoad when new menshen is deployed */
	recommended         []string
	recommendedWithLoad []Load

	/* TODO locations are just used to get the timezone for each gateway. I
	* think it's easier to just merge that info into the version-agnostic
	* Gateway, that is passed from the eipService, and do not worry with
	* the location here */
	locations map[string]Location
}

func (p *gatewayPool) populateCityList() {
	for _, gw := range p.available {
		loc := gw.Location
		gws := p.byCity[loc]
		if len(gws) == 0 {
			p.byCity[loc] = []string{gw.Host}
		} else {
			p.byCity[loc] = append(gws, gw.Host)
		}
	}
	log.Println(p.byCity)
}

func (p *gatewayPool) getCities() []string {
	c := make([]string, 0)
	if p == nil || p.byCity == nil || len(p.byCity) == 0 {
		return c
	}
	if len(p.byCity) != 0 {
		for city := range p.byCity {
			c = append(c, city)
		}
	}
	return c
}

func (p *gatewayPool) isValidCity(city string) bool {
	cities := p.getCities()
	valid := stringInSlice(city, cities)
	return valid
}

/* returns a map of city: gateway for the ui to use */
func (p *gatewayPool) pickGatewayForCities(transport string) map[string]Gateway {
	cities := p.getCities()
	cm := make(map[string]Gateway)
	for _, city := range cities {
		gw, _ := p.getRandomGatewayByCity(city, transport)
		cm[city] = gw
	}
	return cm
}

/* this method should only be used if we have no usable menshen list.
* TODO if we do have an usable menshen list, we can just traverse "recommended"
* and return, in order, at most max gateways for the selected location.
 */
func (p *gatewayPool) getRandomGatewayByCity(city, transport string) (Gateway, error) {
	if !p.isValidCity(city) {
		return Gateway{}, errors.New("bonafide: BUG not a valid city: " + city)
	}
	gws := p.byCity[city]
	if len(gws) == 0 {
		return Gateway{}, errors.New("bonafide: BUG no gw for city " + city)
	}
	s := rand.NewSource(time.Now().Unix())
	r := rand.New(s)
	host := gws[r.Intn(len(gws))]
	for _, gw := range p.available {
		if (gw.Host == host) && (gw.Transport == transport) {
			return gw, nil
		}
	}
	return Gateway{}, errors.New("bonafide: BUG could not find any gateway for that location")
}

/* used when we select a hostname in the ui and we want to know the gateway details */
func (p *gatewayPool) getGatewayByHost(host string) (Gateway, error) {
	for _, gw := range p.available {
		if gw.Host == host {
			return gw, nil
		}
	}
	return Gateway{}, errors.New("bonafide: not a valid host name")
}

/* used when we want to know gateway details after we know what IP openvpn has connected to */
func (p *gatewayPool) getGatewayByIP(ip string) (Gateway, error) {
	for _, gw := range p.available {
		if gw.IPAddress == ip {
			return gw, nil
		}
	}
	return Gateway{}, errors.New("bonafide: not a valid ip address")
}

/* this perhaps could be made more explicit */
func (p *gatewayPool) setAutomaticChoice() {
	p.userChoice = []byte("")
}

/* set a user manual override for gateway location */
func (p *gatewayPool) setUserChoice(city []byte) error {
	if !p.isValidCity(string(city)) {
		return errors.New("bonafide: not a valid city for gateway choice")
	}
	p.userChoice = city
	return nil
}

/* set the recommended field from an ordered array. needs to be modified if menshen passed an array of Loads */
func (p *gatewayPool) setRecommendedGateways(hostnames []string) {
	hosts := make([]string, 0)
	for _, gw := range p.available {
		hosts = append(hosts, gw.Host)
	}

	for _, host := range hostnames {
		if !stringInSlice(host, hosts) {
			log.Println("ERROR: invalid host in recommended list of hostnames", host)
			return
		}
	}

	p.recommended = hostnames
}

/* get at most max gateways. the method of picking depends on whether we're
* doing manual override, and if we got useful info from menshen */
func (p *gatewayPool) getBest(transport string, tz, max int) ([]Gateway, error) {
	gws := make([]Gateway, 0)
	if len(p.userChoice) != 0 {
		/* FIXME this is random because we still do not get menshen to return us load. after "new" menshen is deployed,
		   we can just get them by the order menshen returned */
		gw, err := p.getRandomGatewayByCity(string(p.userChoice), transport)
		gws = append(gws, gw)
		return gws, err
	} else if len(p.recommended) != 0 {
		return p.getGatewaysFromMenshen(transport, max)
	} else {
		return p.getGatewaysByTimezone(transport, tz, max)
	}
}

func (p *gatewayPool) getAll(transport string, tz int) ([]Gateway, error) {
	if len(p.recommended) != 0 {
		return p.getGatewaysFromMenshen(transport, 999)
	}
	return p.getGatewaysByTimezone(transport, tz, 999)
}

/* picks at most max gateways, filtering by transport, from the ordered list menshen returned */
func (p *gatewayPool) getGatewaysFromMenshen(transport string, max int) ([]Gateway, error) {
	gws := make([]Gateway, 0)
	for _, host := range p.recommended {
		for _, gw := range p.available {
			if gw.Transport != transport {
				continue
			}
			if gw.Host == host {
				gws = append(gws, gw)
			}
			if len(gws) == max {
				goto end
			}
		}
	}
end:
	return gws, nil
}

/* the old timezone based heuristic, when everything goes wrong */
func (p *gatewayPool) getGatewaysByTimezone(transport string, tzOffsetHours, max int) ([]Gateway, error) {
	gws := make([]Gateway, 0)
	gwVector := []gatewayDistance{}

	for _, gw := range p.available {
		if gw.Transport != transport {
			continue
		}
		distance := 13
		gwOffset, err := strconv.Atoi(p.locations[gw.Location].Timezone)
		if err != nil {
			log.Printf("Error sorting gateways: %v", err)
			return gws, err
		}
		distance = tzDistance(tzOffsetHours, gwOffset)
		gwVector = append(gwVector, gatewayDistance{gw, distance})
	}
	rand.Seed(time.Now().UnixNano())
	cmp := func(i, j int) bool {
		if gwVector[i].distance == gwVector[j].distance {
			return rand.Intn(2) == 1
		}
		return gwVector[i].distance < gwVector[j].distance
	}
	sort.Slice(gwVector, cmp)

	for _, gw := range gwVector {
		gws = append(gws, gw.gateway)
		if len(gws) == max {
			break
		}
	}
	return gws, nil
}

func newGatewayPool(eip *eipService) *gatewayPool {
	p := gatewayPool{}
	p.available = eip.getGateways()
	p.locations = eip.Locations
	p.byCity = make(map[string][]string)
	p.populateCityList()
	return &p
}

func tzDistance(offset1, offset2 int) int {
	abs := func(x int) int {
		if x < 0 {
			return -x
		}
		return x
	}
	distance := abs(offset1 - offset2)
	if distance > 12 {
		distance = 24 - distance
	}
	return distance
}

func stringInSlice(a string, list []string) bool {
	for _, b := range list {
		if b == a {
			return true
		}
	}
	return false
}