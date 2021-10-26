// Package ownershipvoucher implements Ownershipvoucher deserialization from CBOR
// As for our needs we'll deserialize its header only
package ownershipvoucher

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"

	"github.com/fxamacker/cbor/v2"
	"github.com/redhatinsights/edge-api/pkg/models"
	log "github.com/sirupsen/logrus"
)

func init() {
	log.SetFormatter(&log.JSONFormatter{})
	log.SetOutput(os.Stdout)
}

// CBOR unmarshal of OV header, receives []byte from unmarshalOwnershipVoucher
// returns OV header as pointer to OwnershipVoucherHeader struct & err
func unmarshalOwnershipVoucherHeader(ovhb []byte) (*models.OwnershipVoucherHeader, error) {
	var ovh models.OwnershipVoucherHeader
	err := cbor.Unmarshal(ovhb, &ovh)
	return &ovh, err
}

// If CBOR unmarshal fails => panic
// Something might be wrong with OV
func unmarshalCheck(e error, ovORovh string) {
	if e != nil {
		panic(map[string]interface{}{
			"method":        "ownershipvoucher.unmarshalCheck",
			"what":          ovORovh,
			"error_details": e.Error(),
		})
	}
}

// ParseBytes is CBOR unmarshal of OV, receives []byte from loading the OV file (either reading/receiving)
// do some validation checks and returns OV header as pointer to OwnershipVoucherHeader struct
func ParseBytes(ovb []byte) (ovha []models.OwnershipVoucherHeader, err error) {
	var (
		ov        models.OwnershipVoucher
		counter   int        = 0
		logFields log.Fields = map[string]interface{}{"method": "ownershipvoucher.ParseBytes"}
	)
	defer func() { // in a panic case, stop the parsing but keep alive
		if recErr := recover(); recErr != nil {
			logFields["ovs_parsed"] = counter
			logFields["error_code"] = "parse_error"
			logFields["error_details"] = recErr
			log.WithFields(logFields).Error("panic occurred")
			ejson, _ := json.Marshal(logFields)
			err = errors.New(string(ejson))
		}
	}()
	if err := cbor.Valid(ovb); err == nil { // checking whether the CBOR data is complete and well-formed
		dec := cbor.NewDecoder(bytes.NewReader(ovb))
		for { // stream OVs
			if decErr := dec.Decode(&ov); decErr == io.EOF {
				break
			} else if decErr != nil { // couldn't decode into ownershipvoucher
				unmarshalCheck(decErr, "ownershipvoucher")
				return ovha, decErr
			}
			singleOvh, err := unmarshalOwnershipVoucherHeader(ov.Header)
			unmarshalCheck(err, "ownershipvoucher header")
			ovha = append(ovha, *singleOvh)
			counter++
		}
	} else {
		logFields["ovs_parsed"] = counter
		logFields["error_code"] = "non_ended_voucher"
		logFields["error_details"] = "invalid ownershipvoucher bytes"
		log.WithFields(logFields).Error("Invalid ownershipvoucher bytes")
		ejson, _ := json.Marshal(logFields)
		return nil, errors.New(string(ejson))
	}
	logFields["ovs_parsed"] = counter
	log.WithFields(logFields).Infof("%d ownershipvouchers parsed successfully", counter)
	return ovha, nil
}

// MinimumParse gets one or more OVs as []byte,
// parse them & extract minimum data required without marshal the whole
// OV header to JSON (though possible)
func MinimumParse(ovb []byte) ([]map[string]interface{}, error) {
	ovh, err := ParseBytes(ovb)
	var minimumDataReq []map[string]interface{}
	for _, header := range ovh {
		data := models.ExtractMinimumData(&header)
		minimumDataReq = append(minimumDataReq, data)
		data["method"] = "ownershipvoucher.MinimumParse"
		log.WithFields(data).Debug("New device added")
	}
	return minimumDataReq, err
}