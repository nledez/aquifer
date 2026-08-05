package blobstoretest_test

import (
	"testing"

	"github.com/nledez/aquifer/internal/blobstore"
	"github.com/nledez/aquifer/internal/blobstore/blobstoretest"
)

func TestMemStoreSatisfiesTheContract(t *testing.T) {
	t.Parallel()

	blobstoretest.RunStoreContract(t, func(*testing.T) blobstore.Store {
		return blobstoretest.NewMem()
	})
}
