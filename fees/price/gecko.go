/* Mysterium network payment library.
 *
 * Copyright (C) 2021 BlockDev AG
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Lesser General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 * You should have received a copy of the GNU Lesser General Public License
 * along with this program.  If not, see <https://www.gnu.org/licenses/>.
 */

package price

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"strings"
	"time"
)

type currency string

const (
	// ETH - the ethereum currency.
	ETH currency = "ethereum"
	// MYST - the mysterium coin.
	MYST currency = "mysterium"
	// MATIC - the matic coin.
	MATIC currency = "matic-network"
	// USD - the good old dollar.
	USD currency = "usd"
	// EUR - the official currency of the European Union
	EUR currency = "eur"
)

// Gecko represents a gecko REST API client.
type Gecko struct {
	baseURI string
	client  *http.Client
}

// DefaultGeckoURI points to default coingecko api.
const DefaultGeckoURI = "https://api.coingecko.com/api/v3"

// NewGecko returns a new gecko API client.
func NewGecko(baseURI string) *Gecko {
	if !strings.HasSuffix(baseURI, "/") {
		baseURI += "/"
	}
	return &Gecko{
		baseURI: baseURI,
		client: &http.Client{
			Transport: &http.Transport{
				DialContext: (&net.Dialer{
					Timeout:   10 * time.Second,
					KeepAlive: 10 * time.Second,
				}).DialContext,
				TLSHandshakeTimeout:   10 * time.Second,
				ExpectContinueTimeout: 4 * time.Second,
				ResponseHeaderTimeout: 3 * time.Second,
			},
			Timeout: 10 * time.Second,
		},
	}
}

// Prices represents a single coin pricing information.
type Prices map[string]float64

// PriceResponse represents the gecko price response.
type PriceResponse map[string]Prices

// GetPriceInUSD returns the price in USD.
func (pr PriceResponse) GetPriceInUSD(c currency) (float64, bool) {
	v, ok := pr[string(c)]
	if !ok {
		return 0, false
	}
	p, ok := v[string(USD)]
	return p, ok
}

// GetCoinPrice returns the coin price in the given currencies for given coins.
func (g *Gecko) GetCoinPrice(coins []string, vsCurrencies []string) (PriceResponse, error) {
	path := fmt.Sprintf("simple/price?ids=%v&vs_currencies=%v", strings.Join(coins, ","), strings.Join(vsCurrencies, ","))
	url := fmt.Sprintf("%v%v", g.baseURI, path)
	fmt.Println("calling ", url)
	resp, err := g.client.Get(url)
	if err != nil {
		return PriceResponse{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := ioutil.ReadAll(resp.Body)
		return PriceResponse{}, fmt.Errorf("got an unexpected error code %v, body: %v", resp.StatusCode, string(body))
	}

	var res PriceResponse
	if err = json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return PriceResponse{}, err
	}

	return res, nil
}
