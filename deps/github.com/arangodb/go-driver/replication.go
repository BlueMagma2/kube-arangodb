//
// DISCLAIMER
//
// Copyright 2018 ArangoDB GmbH, Cologne, Germany
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// Copyright holder is ArangoDB GmbH, Cologne, Germany
//
// Author Ewout Prangsma
//

package driver

import (
	"context"
)

// Replication provides access to replication related operations.
type Replication interface {
	// Get the inventory of the server containing all collections (with entire details) of a database.
	// When this function is called on a coordinator is a cluster, an ID of a DBServer must be provided
	// using a context that is prepare with `WithDBServerID`.
	DatabaseInventory(ctx context.Context, db Database) (DatabaseInventory, error)
}
