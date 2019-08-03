package main

import (
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"github.com/dpb587/google-photos-exporter/photoslibrary/v1"
	"github.com/pkg/errors"
	"golang.org/x/net/context"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

func main() {
	b, err := ioutil.ReadFile("credentials.json")
	if err != nil {
		log.Fatalf("Unable to read client secret file: %v", err)
	}

	// If modifying these scopes, delete your previously saved token.json.
	config, err := google.ConfigFromJSON(b, photoslibrary.PhotoslibraryReadonlyScope)
	if err != nil {
		log.Fatalf("Unable to parse client secret file to config: %v", err)
	}
	client := getClient(config)

	srv, err := photoslibrary.New(client)
	if err != nil {
		log.Fatalf("Unable to retrieve Drive client: %v", err)
	}

	r, err := srv.Albums.List().PageSize(25).Do()
	if err != nil {
		log.Fatalf("Unable to retrieve albums: %v", err)
	}

	if len(r.Albums) == 0 {
		fmt.Println("No albums found.")
	} else {
		for _, i := range r.Albums {
			if i.Title != os.Args[1] {
				fmt.Printf("SKIP: %s %s\n", i.Id, i.Title)

				continue
			}

			err = exportAlbum(srv, i, ExportOptions{ContentDirectory: os.Args[2], GraphURI: os.Args[3]})
			if err != nil {
				panic(err)
			}
		}
	}
}

type ExportOptions struct {
	ContentDirectory string
	GraphURI         string
	StaticDirectory  string
}

func exportAlbum(service *photoslibrary.Service, album *photoslibrary.Album, opts ExportOptions) error {
	fmt.Printf("%s %s\n", album.Id, album.Title)

	search := &photoslibrary.SearchMediaItemsRequest{}
	search.AlbumId = album.Id
	search.PageSize = 100

	albumStructuredData := map[string]interface{}{
		"@type":          "Collection",
		"additionalType": "http://schema.org/ItemList",
		"name":           album.Title,
		"sameAs":         fmt.Sprintf("https://photoslibrary.googleapis.com/v1/albums/%s", album.Id),
	}

	dateCreatedMax, _ := time.Parse("2006-01-02", "0999-01-01")
	dateCreatedMin, _ := time.Parse("2006-01-02", "2999-12-31")
	itemListElement := []map[string]string{}

	for {
		res, err := service.MediaItems.Search(search).Do()
		if err != nil {
			return errors.Wrap(err, "searching")
		}

		for _, item := range res.MediaItems {
			fmt.Printf("%s %s %s\n", item.Id, item.MediaMetadata.CreationTime, item.Filename)

			photographStructuredData := map[string]interface{}{
				"@type":  "Photograph",
				"sameAs": fmt.Sprintf("https://photoslibrary.googleapis.com/v1/mediaItems/%s", item.Id),
			}

			{
				dateCreated, err := time.Parse(time.RFC3339, item.MediaMetadata.CreationTime)
				if err != nil {
					return errors.Wrap(err, "parsing time")
				}

				if dateCreated.Before(dateCreatedMin) {
					dateCreatedMin = dateCreated
				}

				if dateCreated.After(dateCreatedMax) {
					dateCreatedMax = dateCreated
				}

				associatedMedia := map[string]interface{}{
					"@type":       "ImageObject",
					"name":        item.Filename,
					"height":      item.MediaMetadata.Height,
					"width":       item.MediaMetadata.Width,
					"dateCreated": dateCreated.Format(time.RFC3339),
				}

				{
					var exifData []map[string]interface{}

					if item.MediaMetadata.Photo.CameraMake != "" {
						exifData = append(
							exifData,
							map[string]interface{}{
								"@type":      "PropertyValue",
								"identifier": "make",
								"value":      item.MediaMetadata.Photo.CameraMake,
							},
						)
					}

					if item.MediaMetadata.Photo.CameraModel != "" {
						exifData = append(
							exifData,
							map[string]interface{}{
								"@type":      "PropertyValue",
								"identifier": "model",
								"value":      item.MediaMetadata.Photo.CameraModel,
							},
						)
					}

					if item.MediaMetadata.Photo.ApertureFNumber != 0 {
						exifData = append(
							exifData,
							map[string]interface{}{
								"@type":      "PropertyValue",
								"identifier": "aperture",
								"value":      strings.TrimRight(fmt.Sprintf("f/%f", item.MediaMetadata.Photo.ApertureFNumber), "0"),
							},
						)
					}

					if item.MediaMetadata.Photo.ExposureTime != "" {
						exifData = append(
							exifData,
							map[string]interface{}{
								"@type":      "PropertyValue",
								"identifier": "exposure",
								"value":      item.MediaMetadata.Photo.ExposureTime,
							},
						)
					}

					if item.MediaMetadata.Photo.IsoEquivalent != 0 {
						exifData = append(
							exifData,
							map[string]interface{}{
								"@type":      "PropertyValue",
								"identifier": "iso",
								"value":      item.MediaMetadata.Photo.IsoEquivalent,
							},
						)
					}

					if len(exifData) > 0 {
						associatedMedia["exifData"] = exifData
					}
				}

				{
					var thumbnail []map[string]interface{}

					thumbnail = append(
						thumbnail,
						map[string]interface{}{
							"@type":      "ImageObject",
							"contentUrl": fmt.Sprintf("%s=w1280-h960", item.BaseUrl),
							"height":     960,
							"width":      1280,
						},
						map[string]interface{}{
							"@type":      "ImageObject",
							"contentUrl": fmt.Sprintf("%s=w640-h480", item.BaseUrl),
							"height":     480,
							"width":      640,
						},
						map[string]interface{}{
							"@type":      "ImageObject",
							"contentUrl": fmt.Sprintf("%s=w200-h200-c", item.BaseUrl),
							"height":     200,
							"width":      200,
						},
						map[string]interface{}{
							"@type":      "ImageObject",
							"contentUrl": fmt.Sprintf("%s=w96-h96-c", item.BaseUrl),
							"height":     96,
							"width":      96,
						},
					)

					associatedMedia["thumbnail"] = thumbnail
				}

				photographStructuredData["associatedMedia"] = associatedMedia
			}

			bucket := sha1.New()
			bucket.Write([]byte(item.Id))

			itemGraphPath := fmt.Sprintf("items/%s/%s.json", fmt.Sprintf("%x", bucket.Sum(nil))[0:2], item.Id)

			{
				b, err := json.MarshalIndent(photographStructuredData, "", "  ")
				if err != nil {
					return errors.Wrap(err, "marshalling")
				}

				lp := path.Join(opts.ContentDirectory, itemGraphPath)

				err = os.MkdirAll(path.Dir(lp), 0700)
				if err != nil {
					return errors.Wrap(err, "mkdir")
				}

				err = ioutil.WriteFile(lp, []byte(fmt.Sprintf("%s\n", b)), 0755)
				if err != nil {
					return errors.Wrap(err, "writing item")
				}
			}

			itemListElement = append(
				itemListElement,
				map[string]string{
					"@id": fmt.Sprintf("%s/%s", opts.GraphURI, itemGraphPath),
				},
			)
		}

		if res.NextPageToken == "" {
			break
		}

		search.PageToken = res.NextPageToken
	}

	albumStructuredData["itemListElement"] = itemListElement

	{
		dateCreatedMinStr := dateCreatedMin.Format("2006-01-02")
		dateCreatedMaxStr := dateCreatedMax.Format("2006-01-02")

		temporalCoverage := dateCreatedMinStr

		if dateCreatedMinStr != dateCreatedMaxStr {
			temporalCoverage = fmt.Sprintf("%s/%s", dateCreatedMinStr, dateCreatedMaxStr)
		}

		albumStructuredData["temporalCoverage"] = temporalCoverage
	}

	{
		albumGraphPath := fmt.Sprintf("albums/%s.json", album.Id)

		b, err := json.MarshalIndent(albumStructuredData, "", "  ")
		if err != nil {
			return errors.Wrap(err, "marshalling")
		}

		lp := path.Join(opts.ContentDirectory, albumGraphPath)

		err = os.MkdirAll(path.Dir(lp), 0700)
		if err != nil {
			return errors.Wrap(err, "mkdir")
		}

		err = ioutil.WriteFile(lp, []byte(fmt.Sprintf("%s\n", b)), 0755)
		if err != nil {
			return errors.Wrap(err, "writing item")
		}
	}

	return nil
}

//
// 	album, err := srv.Albums.Get(os.Args[1]).Do()
// 	if err != nil {
// 		panic(err)
// 	}
//
// 	fmt.Println(album.Title)
//
// 	return
//
// 	m, err := srv.MediaItems.List().Do()
// 	if err != nil {
// 		log.Fatalf("Unable to retrieve media items: %v", err)
// 	}
// 	for _, m := range m.MediaItems {
// 		fmt.Printf("Photo: %s\n", m.BaseUrl)
// 	}
//
// }

// Retrieve a token, saves the token, then returns the generated client.
func getClient(config *oauth2.Config) *http.Client {
	// The file token.json stores the user's access and refresh tokens, and is
	// created automatically when the authorization flow completes for the first
	// time.
	tokFile := "token.json"
	tok, err := tokenFromFile(tokFile)
	if err != nil {
		tok = getTokenFromWeb(config)
		saveToken(tokFile, tok)
	}
	return config.Client(context.Background(), tok)
}

// Request a token from the web, then returns the retrieved token.
func getTokenFromWeb(config *oauth2.Config) *oauth2.Token {
	authURL := config.AuthCodeURL("state-token", oauth2.AccessTypeOffline)
	fmt.Printf("Go to the following link in your browser then type the "+
		"authorization code: \n%v\n", authURL)

	var authCode string
	if _, err := fmt.Scan(&authCode); err != nil {
		log.Fatalf("Unable to read authorization code %v", err)
	}

	tok, err := config.Exchange(context.TODO(), authCode)
	if err != nil {
		log.Fatalf("Unable to retrieve token from web %v", err)
	}
	return tok
}

// Retrieves a token from a local file.
func tokenFromFile(file string) (*oauth2.Token, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tok := &oauth2.Token{}
	err = json.NewDecoder(f).Decode(tok)
	return tok, err
}

// Saves a token to a file path.
func saveToken(path string, token *oauth2.Token) {
	fmt.Printf("Saving credential file to: %s\n", path)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		log.Fatalf("Unable to cache oauth token: %v", err)
	}
	defer f.Close()
	json.NewEncoder(f).Encode(token)
}
