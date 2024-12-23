package resources

import (
	"context"
	"errors"
	"fmt"
	"github.com/Azure/azure-kusto-go/azkustodata"
	dataErrors "github.com/Azure/azure-kusto-go/azkustodata/errors"
	v1 "github.com/Azure/azure-kusto-go/azkustodata/query/v1"
	"io"

	"github.com/Azure/azure-kusto-go/azkustodata/types"
	"github.com/Azure/azure-kusto-go/azkustodata/value"
	"github.com/Azure/azure-kusto-go/azkustoingest/internal/properties"
)

type FakeMgmt struct {
	DBEqual    string
	QueryEqual string
	err        error
	dataset    v1.Dataset
}

func FakeResources(rows []value.Values, setErr bool) *FakeMgmt {
	cols := []v1.RawColumn{
		{ColumnName: "ResourceTypeName", ColumnType: string(types.String)},
		{ColumnName: "StorageRoot", ColumnType: string(types.String)},
	}

	fm := NewFakeMgmt(cols, rows, setErr)
	return fm
}

func NewFakeMgmt(columns []v1.RawColumn, vals []value.Values, setErr bool) *FakeMgmt {
	outRows := make([]v1.RawRow, 0, len(vals))
	for _, rowVal := range vals {
		row := make([]interface{}, 0, len(rowVal))
		for _, val := range rowVal {
			row = append(row, val.GetValue())
		}
		outRows = append(outRows, v1.RawRow{Row: row})
	}

	var err error
	if setErr {
		err = dataErrors.ES(dataErrors.OpMgmt, dataErrors.KOther, "some error")
	}

	dataset, _ := v1.NewDataset(context.Background(), dataErrors.OpMgmt, v1.V1{
		Tables: []v1.RawTable{
			{
				TableName: "Table",
				Columns:   columns,
				Rows:      outRows,
			},
		}})
	return &FakeMgmt{
		dataset: dataset,
		err:     err,
	}
}

func (f *FakeMgmt) SetDBEquals(s string) *FakeMgmt {
	f.DBEqual = s
	return f
}

func (f *FakeMgmt) SetQueryEquals(s string) *FakeMgmt {
	f.DBEqual = s
	return f
}

func (f *FakeMgmt) SetMgmtErr() *FakeMgmt {
	f.err = errors.New("Set some error")
	return f
}

func (f *FakeMgmt) Mgmt(_ context.Context, db string, query azkustodata.Statement, _ ...azkustodata.QueryOption) (v1.Dataset, error) {
	if f.DBEqual != "" {
		if db != f.DBEqual {
			panic(fmt.Sprintf("expected db to be %q, was %q", f.DBEqual, db))
		}
	}
	if f.QueryEqual != "" {
		if query.String() != f.QueryEqual {
			panic(fmt.Sprintf("expected query to be %q, was %q", f.QueryEqual, db))
		}
	}

	if f.err != nil {
		return nil, f.err
	}

	return f.dataset, nil
}

func SuccessfulFakeResources() *FakeMgmt {
	return FakeResources(
		[]value.Values{
			{
				value.NewString("TempStorage"),
				value.NewString("https://account.blob.core.windows.net/storageroot0"),
			},
			{
				value.NewString("SecuredReadyForAggregationQueue"),
				value.NewString("https://account.blob.core.windows.net/storageroot1"),
			},
		},
		false,
	)
}

type FsMock struct {
	OnLocal  func(ctx context.Context, from string, props properties.All) error
	OnReader func(ctx context.Context, reader io.Reader, props properties.All) (string, error)
	OnBlob   func(ctx context.Context, from string, fileSize int64, props properties.All) error
}

func (f FsMock) Close() error {
	return nil
}

func (f FsMock) Local(ctx context.Context, from string, props properties.All) error {
	if f.OnLocal != nil {
		return f.OnLocal(ctx, from, props)
	}
	return nil
}

func (f FsMock) Reader(ctx context.Context, reader io.Reader, props properties.All) (string, error) {
	if f.OnReader != nil {
		return f.OnReader(ctx, reader, props)
	}
	return "", nil
}

func (f FsMock) Blob(ctx context.Context, from string, fileSize int64, props properties.All) error {
	if f.OnBlob != nil {
		return f.OnBlob(ctx, from, fileSize, props)
	}
	return nil
}
