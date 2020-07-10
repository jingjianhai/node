/*
 * Copyright (C) 2020 The "MysteriumNetwork/node" Authors.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

package contract

import (
	"time"

	"github.com/mysteriumnetwork/node/consumer/session"
)

// NewSessionListResponse maps to API session list.
func NewSessionListResponse(sessions []session.History) ListSessionsResponse {
	return ListSessionsResponse{
		Sessions: mapSessions(sessions, NewSessionDTO),
	}
}

// ListSessionsResponse defines session list representable as json
// swagger:model ListSessionsResponse
type ListSessionsResponse struct {
	Sessions []SessionDTO `json:"sessions"`
}

func mapSessions(sessions []session.History, f func(session.History) SessionDTO) []SessionDTO {
	dtoArray := make([]SessionDTO, len(sessions))
	for i, se := range sessions {
		dtoArray[i] = f(se)
	}
	return dtoArray
}

// NewSessionDTO maps to API session.
func NewSessionDTO(se session.History) SessionDTO {
	return SessionDTO{
		ID:              string(se.SessionID),
		Direction:       se.Direction,
		ConsumerID:      se.ConsumerID.Address,
		AccountantID:    se.AccountantID,
		ProviderID:      se.ProviderID.Address,
		ServiceType:     se.ServiceType,
		ProviderCountry: se.ProviderCountry,
		CreatedAt:       se.Started.Format(time.RFC3339),
		BytesReceived:   se.DataReceived,
		BytesSent:       se.DataSent,
		Duration:        uint64(se.GetDuration().Seconds()),
		Tokens:          se.Tokens,
		Status:          se.Status,
	}
}

// SessionDTO represents the session object
// swagger:model SessionDTO
type SessionDTO struct {
	// example: 4cfb0324-daf6-4ad8-448b-e61fe0a1f918
	ID string `json:"id"`

	// example: Consumer
	Direction string `json:"direction"`

	// example: 0x0000000000000000000000000000000000000001
	ConsumerID string `json:"consumer_id"`

	// example: 0x0000000000000000000000000000000000000001
	AccountantID string `json:"accountant_id"`

	// example: 0x0000000000000000000000000000000000000001
	ProviderID string `json:"provider_id"`

	// example: openvpn
	ServiceType string `json:"service_type"`

	// example: NL
	ProviderCountry string `json:"provider_country"`

	// example: 2019-06-06T11:04:43.910035Z
	CreatedAt string `json:"created_at"`

	// duration in seconds
	// example: 120
	Duration uint64 `json:"duration"`

	// example: 1024
	BytesReceived uint64 `json:"bytes_received"`

	// example: 1024
	BytesSent uint64 `json:"bytes_sent"`

	// example: 500000
	Tokens uint64 `json:"tokens"`

	// example: Completed
	Status string `json:"status"`
}
