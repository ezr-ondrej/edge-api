package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"text/template"
	"time"

	"github.com/redhatinsights/edge-api/config"
	"github.com/redhatinsights/edge-api/pkg/clients/imagebuilder"
	"github.com/redhatinsights/edge-api/pkg/db"
	"github.com/redhatinsights/edge-api/pkg/errors"
	"github.com/redhatinsights/edge-api/pkg/models"
	"github.com/redhatinsights/edge-api/pkg/routes/common"
	log "github.com/sirupsen/logrus"

	"gorm.io/gorm"
)

// WaitGroup is the waitg roup for pending image builds
var WaitGroup sync.WaitGroup

// ImageServiceInterface defines the interface that helps handle
// the business logic of creating RHEL For Edge Images
type ImageServiceInterface interface {
	CreateImage(image *models.Image, account string) error
	UpdateImage(image *models.Image, previousImage *models.Image) error
	AddUserInfo(image *models.Image) error
	UpdateImageStatus(image *models.Image) (*models.Image, error)
	SetErrorStatusOnImage(err error, i *models.Image)
	CreateRepoForImage(i *models.Image) (*models.Repo, error)
	GetImageByID(id string) (*models.Image, error)
	GetUpdateInfo(image models.Image) ([]ImageUpdateAvailable, error)
	AddPackageInfo(image *models.Image) (ImageDetail, error)
	GetImageByOSTreeCommitHash(commitHash string) (*models.Image, error)
	CheckImageName(name, account string) (bool, error)
	RetryCreateImage(image *models.Image) error
	GetMetadata(image *models.Image) (*models.Image, error)
}

// NewImageService gives a instance of the main implementation of a ImageServiceInterface
func NewImageService(ctx context.Context, log *log.Entry) ImageServiceInterface {
	return &ImageService{
		Service:      Service{ctx: ctx, log: log.WithField("service", "image")},
		imageBuilder: imagebuilder.InitClient(ctx, log),
		repoBuilder:  NewRepoBuilder(ctx, log),
	}
}

// ImageService is the main implementation of a ImageServiceInterface
type ImageService struct {
	Service

	imageBuilder imagebuilder.ClientInterface
	repoBuilder  RepoBuilderInterface
}

// CreateImage creates an Image for an Account on Image Builder and on our database
func (s *ImageService) CreateImage(image *models.Image, account string) error {
	var imageSet models.ImageSet
	imageSet.Account = account
	imageSet.Name = image.Name
	imageSet.Version = image.Version
	set := db.DB.Create(&imageSet)
	if set.Error == nil {
		image.ImageSetID = &imageSet.ID
	}
	image, err := s.imageBuilder.ComposeCommit(image)
	if err != nil {
		return err
	}
	image.Account = account
	image.Commit.Account = account
	image.Commit.Status = models.ImageStatusBuilding
	image.Status = models.ImageStatusBuilding
	// TODO: Remove code when frontend is not using ImageType on the table
	if image.HasOutputType(models.ImageTypeInstaller) {
		image.ImageType = models.ImageTypeInstaller
	} else {
		image.ImageType = models.ImageTypeCommit
	}
	// TODO: End of remove block
	if image.HasOutputType(models.ImageTypeInstaller) {
		image.Installer.Status = models.ImageStatusCreated
		image.Installer.Account = image.Account
		tx := db.DB.Create(&image.Installer)
		if tx.Error != nil {
			return tx.Error
		}
	}
	tx := db.DB.Create(&image.Commit)
	if tx.Error != nil {
		return tx.Error
	}
	tx = db.DB.Create(&image)
	if tx.Error != nil {
		return tx.Error
	}

	go s.postProcessImage(image.ID)

	return nil
}

// UpdateImage updates an image, adding a new version of this image to an imageset
func (s *ImageService) UpdateImage(image *models.Image, previousImage *models.Image) error {
	if previousImage == nil {
		return new(ImageNotFoundError)
	}
	err := s.checkForDuplicateImageVersion(previousImage)
	if err != nil {
		return errors.NewBadRequest("only the latest updated image can be modified")
	}
	if previousImage.Status == models.ImageStatusSuccess {
		// Previous image was built sucessfully
		var currentImageSet models.ImageSet
		result := db.DB.Where("Id = ?", previousImage.ImageSetID).First(&currentImageSet)
		if result.Error != nil {
			return result.Error
		}
		currentImageSet.Version = currentImageSet.Version + 1
		image.ImageSetID = previousImage.ImageSetID
		if err := db.DB.Save(currentImageSet).Error; err != nil {
			return result.Error
		}
		if image.Commit.OSTreeParentCommit == "" {
			if previousImage.Commit.OSTreeParentCommit != "" {
				image.Commit.OSTreeParentCommit = previousImage.Commit.OSTreeParentCommit
			} else {
				var repo *RepoService
				repoURL, err := repo.GetRepoByID(previousImage.Commit.RepoID)
				if err != nil {
					err := errors.NewBadRequest(fmt.Sprintf("Commit Repo wasn't found in the database: #%v", image.Commit.ID))
					return err
				}
				image.Commit.OSTreeParentCommit = repoURL.URL
			}
		}
		if image.Commit.OSTreeRef == "" {
			if previousImage.Commit.OSTreeRef != "" {
				image.Commit.OSTreeRef = previousImage.Commit.OSTreeRef
			} else {
				image.Commit.OSTreeRef = config.Get().DefaultOSTreeRef
			}
		}
	} else {
		// Previous image was not built sucessfully
		s.log.WithField("previousImageID", previousImage.ID).Info("Creating an update based on a image with a status that is not success")
	}
	image, err = s.imageBuilder.ComposeCommit(image)
	if err != nil {
		return err
	}
	image.Account = previousImage.Account
	image.Commit.Account = previousImage.Account
	image.Commit.Status = models.ImageStatusBuilding
	image.Status = models.ImageStatusBuilding
	// TODO: Remove code when frontend is not using ImageType on the table
	if image.HasOutputType(models.ImageTypeInstaller) {
		image.ImageType = models.ImageTypeInstaller
	} else {
		image.ImageType = models.ImageTypeCommit
	}
	// TODO: End of remove block
	if image.HasOutputType(models.ImageTypeInstaller) {
		image.Installer.Status = models.ImageStatusCreated
		image.Installer.Account = image.Account
		tx := db.DB.Create(&image.Installer)
		if tx.Error != nil {
			return tx.Error
		}
	}
	tx := db.DB.Create(&image.Commit)
	if tx.Error != nil {
		return tx.Error
	}
	tx = db.DB.Create(&image)
	if tx.Error != nil {
		return tx.Error
	}

	s.log = s.log.WithFields(log.Fields{"imageID": image.ID, "commitID": image.Commit.ID})
	go s.postProcessImage(image.ID)

	return nil
}

func (s *ImageService) postProcessInstaller(i *models.Image) error {
	s.log.Debug("Post processing installer")
	i, err := s.imageBuilder.ComposeInstaller(i)
	if err != nil {
		return err
	}
	i.Installer.Status = models.ImageStatusBuilding
	tx := db.DB.Save(&i.Installer)
	if tx.Error != nil {
		s.log.WithField("error", err.Error()).Error("Error setting installer building status")
		return err
	}
	for {
		i, err := s.UpdateImageStatus(i)
		if err != nil {
			s.log.WithField("error", err.Error()).Error("Update image status error")
			return err
		}
		if i.Installer.Status != models.ImageStatusBuilding {
			break
		}
		time.Sleep(1 * time.Minute)
	}

	if i.Installer.Status == models.ImageStatusSuccess {
		err = s.AddUserInfo(i)
		if err != nil {
			s.log.WithField("error", err.Error()).Error("Kickstart file injection failed")
			return err
		}
	}
	return nil
}

func (s *ImageService) postProcessCommit(image *models.Image) error {
	s.log.Debug("Post processing commit")
	for {
		i, err := s.UpdateImageStatus(image)
		if err != nil {
			s.log.WithField("error", err.Error()).Error("Update image status error")
			return err
		}
		if i.Commit.Status != models.ImageStatusBuilding {
			break
		}
		time.Sleep(1 * time.Minute)
	}

	if image.Commit.Status == models.ImageStatusSuccess {
		i, err := s.imageBuilder.GetMetadata(image)
		if err != nil {
			s.log.WithField("error", err.Error()).Error("Failed getting metadata from image builder")
			s.SetErrorStatusOnImage(err, i)
			return err
		}

		_, err = s.CreateRepoForImage(image)
		if err != nil {
			s.log.WithField("error", err.Error()).Error("Failed creating repo for image")
			return err
		}
	}
	return nil
}

func (s *ImageService) setFinalImageStatus(i *models.Image) error {
	// image status can be success if all output types are successful
	// if any status are not final (success/error) then sets to error
	// image status is error if any output status is error
	success := true
	for _, out := range i.OutputTypes {
		if out == models.ImageTypeCommit {
			if i.Commit == nil || i.Commit.Status != models.ImageStatusSuccess {
				success = false
			}
			if i.Commit.Status == models.ImageStatusBuilding {
				success = false
				i.Commit.Status = models.ImageStatusError
				db.DB.Save(i.Commit)
			}
		}
		if out == models.ImageTypeInstaller {
			if i.Installer == nil || i.Installer.Status != models.ImageStatusSuccess {
				success = false
			}
			if i.Installer.Status == models.ImageStatusBuilding {
				success = false
				i.Installer.Status = models.ImageStatusError
				db.DB.Save(i.Installer)
			}
		}
	}

	if success {
		i.Status = models.ImageStatusSuccess
	} else {
		i.Status = models.ImageStatusError
	}

	tx := db.DB.Save(i)
	return tx.Error
}

// Every log message in this method already has commit id and image id injected
func (s *ImageService) postProcessImage(id uint) {
	s.log.Debug("Post processing image")
	var i *models.Image
	db.DB.Joins("Commit").Joins("Installer").First(&i, id)

	WaitGroup.Add(1) // Processing one image
	defer func() {
		WaitGroup.Done() // Done with one image (successfuly or not)
		s.log.Debug("Done with one image - successfuly or not")
		if err := recover(); err != nil {
			s.log.Fatalf("%s", err)
		}
	}()
	go func(i *models.Image) {
		sigint := make(chan os.Signal, 1)
		signal.Notify(sigint, os.Interrupt, syscall.SIGTERM)
		sig := <-sigint
		// Reload image to get updated status
		db.DB.Joins("Commit").Joins("Installer").First(&i, i.ID)
		if i.Status == models.ImageStatusBuilding {
			s.log.WithField("signal", sig).Info("Captured signal marking image as error", sig)
			s.SetErrorStatusOnImage(nil, i)
			WaitGroup.Done()
		}
	}(i)

	err := s.postProcessCommit(i)
	if err != nil {
		s.SetErrorStatusOnImage(err, i)
		s.log.WithField("error", err.Error()).Fatal("Failed creating commit for image")
	}

	if i.Commit.Status == models.ImageStatusSuccess {
		s.log.Debug("Commit is successful")
		// TODO: We need to discuss this whole thing post-July deliverable
		if i.HasOutputType(models.ImageTypeInstaller) {
			err = s.postProcessInstaller(i)
			if err != nil {
				s.SetErrorStatusOnImage(err, i)
				s.log.WithField("error", err.Error()).Fatal("Failed creating installer for image")
			}
		} else {
			// Cleaning up possible non nil installers if doesnt have output type installer
			i.Installer = nil
		}
	}
	s.log.Debug("Setting final image status")
	err = s.setFinalImageStatus(i)
	if err != nil {
		s.log.WithField("error", err.Error()).Fatal("Couldn't set final image status")
	}
	s.log.Debug("Post processing image is done")
}

// CreateRepoForImage creates the OSTree repo to host that image
func (s *ImageService) CreateRepoForImage(i *models.Image) (*models.Repo, error) {
	s.log.Infof("Creating OSTree repo.")
	repo := &models.Repo{
		Status: models.RepoStatusBuilding,
	}
	tx := db.DB.Create(repo)
	if tx.Error != nil {
		return nil, tx.Error
	}
	s.log = s.log.WithField("repoID", repo.ID)
	s.log.Debug("OSTree repo is created on the database")

	i.Commit.Repo = repo
	i.Commit.RepoID = &repo.ID

	tx = db.DB.Save(i.Commit)
	if tx.Error != nil {
		return nil, tx.Error
	}
	log.Debug("OSTree repo was saved to commit")

	repo, err := s.repoBuilder.ImportRepo(repo)
	if err != nil {
		return nil, err
	}
	log.Infof("OSTree repo is ready")

	return repo, nil
}

// SetErrorStatusOnImage is a helper functions that sets the error status on images
func (s *ImageService) SetErrorStatusOnImage(err error, i *models.Image) {
	if i.Status != models.ImageStatusError {
		i.Status = models.ImageStatusError
		tx := db.DB.Save(i)
		s.log.Debug("Image saved with error status")
		if tx.Error != nil {
			s.log.Error(tx.Error)
			panic(tx.Error)
		}
		if i.Commit != nil {
			i.Commit.Status = models.ImageStatusError
			tx := db.DB.Save(i.Commit)
			if tx.Error != nil {
				s.log.Error(tx.Error)
				panic(tx.Error)
			}
		}
		if i.Installer != nil {
			i.Installer.Status = models.ImageStatusError
			tx := db.DB.Save(i.Installer)
			if tx.Error != nil {
				s.log.Error(tx.Error)
				panic(tx.Error)
			}
		}
		if err != nil {
			s.log.Error(err)
			panic(err)
		}
	}
}

// AddUserInfo downloads the ISO
// injects the kickstart with username and ssh key
// and then re-uploads the ISO into our bucket
func (s *ImageService) AddUserInfo(image *models.Image) error {
	// Absolute path for manipulating ISO's
	destPath := "/var/tmp/"

	downloadURL := image.Installer.ImageBuildISOURL
	sshKey := image.Installer.SSHKey
	username := image.Installer.Username
	// Files that will be used to modify the ISO and will be cleaned
	imageName := destPath + image.Name
	kickstart := fmt.Sprintf("%sfinalKickstart-%s_%d.ks", destPath, image.Account, image.ID)

	err := s.downloadISO(imageName, downloadURL)
	if err != nil {
		return fmt.Errorf("error downloading ISO file :: %s", err.Error())
	}

	err = s.addSSHKeyToKickstart(sshKey, username, kickstart)
	if err != nil {
		return fmt.Errorf("error adding ssh key to kickstart file :: %s", err.Error())
	}

	err = s.exeInjectionScript(kickstart, imageName, image.ID)
	if err != nil {
		return fmt.Errorf("error execuiting fleetkick script :: %s", err.Error())
	}

	err = s.calculateChecksum(imageName, image)
	if err != nil {
		return fmt.Errorf("error calculating checksum for ISO :: %s", err.Error())
	}

	err = s.uploadISO(image, imageName)
	if err != nil {
		return fmt.Errorf("error uploading ISO :: %s", err.Error())
	}

	err = s.cleanFiles(kickstart, imageName, image.ID)
	if err != nil {
		return fmt.Errorf("error cleaning files :: %s", err.Error())
	}

	return nil
}

// UnameSSH is the template struct for username and ssh key
type UnameSSH struct {
	Sshkey   string
	Username string
}

// Adds user provided ssh key to the kickstart file.
func (s *ImageService) addSSHKeyToKickstart(sshKey string, username string, kickstart string) error {
	cfg := config.Get()

	td := UnameSSH{sshKey, username}

	log.Infof("Opening file %s", cfg.TemplatesPath)
	t, err := template.ParseFiles(cfg.TemplatesPath + "templateKickstart.ks")
	if err != nil {
		return err
	}

	log.Infof("Creating file %s", kickstart)
	file, err := os.Create(kickstart)
	if err != nil {
		return err
	}

	log.Infof("Injecting username %s and key %s into template", username, sshKey)
	err = t.Execute(file, td)
	if err != nil {
		return err
	}
	file.Close()

	return nil
}

// Download created ISO into the file system.
func (s *ImageService) downloadISO(isoName string, url string) error {
	log.Infof("Creating iso %s", isoName)
	iso, err := os.Create(isoName)
	if err != nil {
		return err
	}
	defer iso.Close()

	log.Infof("Downloading ISO %s", url)
	res, err := http.Get(url)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	_, err = io.Copy(iso, res.Body)
	if err != nil {
		return err
	}

	return nil
}

// Upload finished ISO to S3
func (s *ImageService) uploadISO(image *models.Image, imageName string) error {

	uploadPath := fmt.Sprintf("%s/isos/%s.iso", image.Account, image.Name)
	filesService := NewFilesService()
	url, err := filesService.GetUploader().UploadFile(imageName, uploadPath)

	if err != nil {
		return fmt.Errorf("error uploading the ISO :: %s :: %s", uploadPath, err.Error())
	}

	image.Installer.ImageBuildISOURL = url
	tx := db.DB.Save(&image.Installer)
	if tx.Error != nil {
		return tx.Error
	}
	return nil
}

// Remove edited kickstart after use.
func (s *ImageService) cleanFiles(kickstart string, isoName string, imageID uint) error {
	err := os.Remove(kickstart)
	if err != nil {
		return err
	}
	log.Info("Kickstart file " + kickstart + " removed!")

	err = os.Remove(isoName)
	if err != nil {
		return err
	}
	log.Info("ISO file " + isoName + " removed!")

	workDir := fmt.Sprintf("/var/tmp/workdir%d", imageID)
	err = os.RemoveAll(workDir)
	if err != nil {
		return err
	}
	log.Info("work dir file " + workDir + " removed!")

	return nil
}

// UpdateImageStatus updates the status of an commit and/or installer based on Image Builder's status
func (s *ImageService) UpdateImageStatus(image *models.Image) (*models.Image, error) {
	if image.Commit.Status == models.ImageStatusBuilding {
		image, err := s.imageBuilder.GetCommitStatus(image)
		if err != nil {
			return image, err
		}
		if image.Commit.Status != models.ImageStatusBuilding {
			tx := db.DB.Save(&image.Commit)
			if tx.Error != nil {
				return image, tx.Error
			}
		}
	}
	if image.Installer != nil && image.Installer.Status == models.ImageStatusBuilding {
		image, err := s.imageBuilder.GetInstallerStatus(image)
		if err != nil {
			return image, err
		}
		if image.Installer.Status != models.ImageStatusBuilding {
			tx := db.DB.Save(&image.Installer)
			if tx.Error != nil {
				return image, tx.Error
			}
		}
	}
	if image.Status != models.ImageStatusBuilding {
		tx := db.DB.Save(&image)
		if tx.Error != nil {
			return image, tx.Error
		}
	}
	return image, nil
}

// CheckImageName returns false if the image doesnt exist and true if the image exists
func (s *ImageService) CheckImageName(name, account string) (bool, error) {
	var imageFindByName *models.Image
	result := db.DB.Where("name = ? AND account = ?", name, account).First(&imageFindByName)
	if result.Error != nil {
		if result.Error == gorm.ErrRecordNotFound {
			return false, nil
		}
		return false, result.Error
	}
	return imageFindByName != nil, nil
}

// Inject the custom kickstart into the iso via script.
func (s *ImageService) exeInjectionScript(kickstart string, image string, imageID uint) error {
	fleetBashScript := "/usr/local/bin/fleetkick.sh"
	workDir := fmt.Sprintf("/var/tmp/workdir%d", imageID)
	err := os.Mkdir(workDir, 0755)
	if err != nil {
		return err
	}

	cmd := exec.Command(fleetBashScript, kickstart, image, image, workDir)
	output, err := cmd.Output()
	if err != nil {
		return err
	}
	log.Infof("fleetkick output: %s\n", output)
	return nil
}

// Calculate the checksum of the final ISO.
func (s *ImageService) calculateChecksum(isoPath string, image *models.Image) error {
	log.Infof("Calculating sha256 checksum for ISO %s", isoPath)

	fh, err := os.Open(isoPath)
	if err != nil {
		return err
	}
	defer fh.Close()

	sumCalculator := sha256.New()
	_, err = io.Copy(sumCalculator, fh)
	if err != nil {
		return err
	}

	image.Installer.Checksum = hex.EncodeToString(sumCalculator.Sum(nil))
	log.Infof("Checksum (sha256): %s", image.Installer.Checksum)
	tx := db.DB.Save(&image.Installer)
	if tx.Error != nil {
		return tx.Error
	}

	return nil
}

//ImageDetail return the structure to inform package info to images
type ImageDetail struct {
	Image              *models.Image `json:"image"`
	AdditionalPackages int           `json:"additional_packages"`
	Packages           int           `json:"packages"`
	UpdateAdded        int           `json:"update_added"`
	UpdateRemoved      int           `json:"update_removed"`
	UpdateUpdated      int           `json:"update_updated"`
}

// AddPackageInfo return info related to packages on image
func (s *ImageService) AddPackageInfo(image *models.Image) (ImageDetail, error) {
	var imgDetail ImageDetail
	imgDetail.Image = image
	imgDetail.Packages = len(image.Commit.InstalledPackages)
	imgDetail.AdditionalPackages = len(image.Packages)

	upd, err := s.GetUpdateInfo(*image)
	if err != nil {
		log.Errorf("error getting update info: %v", err)
		return imgDetail, err
	}
	if upd != nil {
		imgDetail.UpdateAdded = len(upd[len(upd)-1].PackageDiff.Removed)
		imgDetail.UpdateRemoved = len(upd[len(upd)-1].PackageDiff.Added)
		imgDetail.UpdateUpdated = len(upd[len(upd)-1].PackageDiff.Upgraded)
	} else {
		imgDetail.UpdateAdded = 0
		imgDetail.UpdateRemoved = 0
		imgDetail.UpdateUpdated = 0
	}
	return imgDetail, nil
}

func (s *ImageService) addImageExtraData(image *models.Image) (*models.Image, error) {
	if image.InstallerID != nil {
		result := db.DB.First(&image.Installer, image.InstallerID)
		if result.Error != nil {
			s.log.WithField("error", result.Error).Error("Error retrieving installer for image")
			return nil, result.Error
		}
	}
	err := db.DB.Model(image).Association("Packages").Find(&image.Packages)
	if err != nil {
		s.log.WithField("error", err).Error("Error packages from image")
		return nil, err
	}
	return image, nil
}

// GetImageByID retrieves an image by its identifier
func (s *ImageService) GetImageByID(imageID string) (*models.Image, error) {
	var image models.Image
	account, err := common.GetAccountFromContext(s.ctx)
	if err != nil {
		s.log.WithField("error", err).Error("Error retrieving account")
		return nil, new(AccountNotSet)
	}
	id, err := strconv.Atoi(imageID)
	if err != nil {
		s.log.WithField("error", err).Debug("Request related error - ID is not integer")
		return nil, new(IDMustBeInteger)
	}
	result := db.DB.Preload("Commit.Repo").Preload("Commit.InstalledPackages").Where("images.account = ?", account).Joins("Commit").First(&image, id)
	if result.Error != nil {
		s.log.WithField("error", err).Debug("Request related error - image is not found")
		return nil, new(ImageNotFoundError)
	}
	return s.addImageExtraData(&image)
}

// GetImageByOSTreeCommitHash retrieves an image by its ostree commit hash
func (s *ImageService) GetImageByOSTreeCommitHash(commitHash string) (*models.Image, error) {
	s.log.Info("Getting image by OSTreeHash")
	var image models.Image
	account, err := common.GetAccountFromContext(s.ctx)
	if err != nil {
		s.log.Error("Error retreving account")
		return nil, new(AccountNotSet)
	}
	result := db.DB.Where("images.account = ? and os_tree_commit = ?", account, commitHash).Joins("Commit").First(&image)
	if result.Error != nil {
		s.log.WithField("error", result.Error).Error("Error retrieving image by OSTreeHash")
		return nil, new(ImageNotFoundError)
	}
	s.log = s.log.WithField("imageID", image.ID)
	s.log.Info("Image successfuly retrievedd by its OSTreeHash")
	return s.addImageExtraData(&image)
}

// RetryCreateImage retries the whole post process of the image creation
func (s *ImageService) RetryCreateImage(image *models.Image) error {
	s.log = s.log.WithFields(log.Fields{"imageID": image.ID, "commitID": image.Commit.ID})
	// recompose commit
	image, err := s.imageBuilder.ComposeCommit(image)
	if err != nil {
		s.log.WithField("error", err.Error()).Error("Failed recomposing commit")
		return err
	}
	err = s.setBuildingStatusOnImageToRetryBuild(image)
	if err != nil {
		s.log.WithField("error", err.Error()).Error("Failed setting image status")
		return nil
	}
	go s.postProcessImage(image.ID)
	return nil
}

func (s *ImageService) setBuildingStatusOnImageToRetryBuild(image *models.Image) error {
	image.Status = models.ImageStatusBuilding
	if image.Commit != nil {
		image.Commit.Status = models.ImageStatusBuilding
		// Repo will be recreated from scratch, its safer and simpler as this stage
		if image.Commit.Repo != nil {
			image.Commit.Repo = nil
			tx := db.DB.Save(image.Commit.Repo)
			if tx.Error != nil {
				return tx.Error
			}
		}
		tx := db.DB.Save(image.Commit)
		if tx.Error != nil {
			return tx.Error
		}
	}
	if image.Installer != nil {
		image.Installer.Status = models.ImageStatusCreated
		tx := db.DB.Save(image.Installer)
		if tx.Error != nil {
			return tx.Error
		}
	}
	tx := db.DB.Save(image)
	if tx.Error != nil {
		return tx.Error
	}
	return nil
}
func uploadTarRepo(account, imageName string, repoID int) (string, error) {
	log.Infof(":: uploadTarRepo Started ::\n")
	uploadPath := fmt.Sprintf("%s/tar/%v/%s", account, repoID, imageName)
	filesService := NewFilesService()
	url, err := filesService.GetUploader().UploadFile(imageName, uploadPath)

	if err != nil {
		return "error", fmt.Errorf("error uploading the Tar :: %s :: %s", uploadPath, err.Error())
	}
	log.Infof(":: uploadTarRepo Finish ::\n")

	return url, nil
}

// checkForDuplicateImageVersion make sure that there is no same image version present
func (s *ImageService) checkForDuplicateImageVersion(previousImage *models.Image) error {
	/*
		nowImage is the version of current Image which we are updating i.e if we are updating image1 to image 2 then image1 is the nowImage.
		currentHighestImageVersion is the latest version present in the DB of image we're updating.
		nextImageVersion is the next version of current image to which we are updating the image i.e if image3 is the next version of image2.
	*/
	var nowImage *models.Image
	var currentHighestImageVersion *models.Image
	var nextImageVersion *models.Image

	nowVersionImage := db.DB.Where("version = ?", previousImage.Version).First(&nowImage)
	if nowVersionImage.Error != nil {
		return nowVersionImage.Error
	}
	currentVersionImage := db.DB.Select("version").Where("name = ? ", previousImage.Name).Order("version desc").First(&currentHighestImageVersion)
	if currentVersionImage.Error != nil {
		return currentVersionImage.Error
	}
	var compareImageVersion = nowImage.Version + 1
	newImageVersion := db.DB.Select("version").Where("version = ? and name = ? ", compareImageVersion, previousImage.Name).Find(&nextImageVersion)
	if newImageVersion.Error != nil {
		return newImageVersion.Error
	}
	if currentHighestImageVersion.Version == compareImageVersion || nextImageVersion.Version == compareImageVersion {
		return new(ImageVersionAlreadyExists)
	}
	return nil
}

//GetUpdateInfo return package info when has an update to the image
func (s *ImageService) GetUpdateInfo(image models.Image) ([]ImageUpdateAvailable, error) {
	var images []models.Image
	var imageDiff []ImageUpdateAvailable
	updates := db.DB.Where("Image_set_id = ? and Images.Status = ? and Images.Id < ?",
		image.ImageSetID, models.ImageStatusSuccess, image.ID).Joins("Commit").
		Order("Images.updated_at desc").Find(&images)

	if updates.Error != nil {
		s.log.WithField("error", updates.Error.Error()).Error("Error retrieving update")
		return nil, new(UpdateNotFoundError)
	}
	if updates.RowsAffected == 0 {
		s.log.Info("No rows affected")
		return nil, nil
	}
	for _, upd := range images {
		db.DB.First(&upd.Commit, upd.CommitID)
		db.DB.Model(&upd.Commit).Association("InstalledPackages").Find(&upd.Commit.InstalledPackages)
		db.DB.Model(&upd).Association("Packages").Find(&upd.Packages)
		var delta ImageUpdateAvailable
		diff := getDiffOnUpdate(image, upd)
		upd.Commit.InstalledPackages = nil // otherwise the frontend will get the whole list of installed packages
		delta.Image = upd
		delta.PackageDiff = diff
		imageDiff = append(imageDiff, delta)
	}
	return imageDiff, nil
}

//GetMetadata return package info when has an update to the image
func (s *ImageService) GetMetadata(image *models.Image) (*models.Image, error) {
	s.log.Debug("Retrieving metadata")
	image, err := s.imageBuilder.GetMetadata(image)
	if err != nil {
		s.log.WithField("error", err.Error()).Error("Error retrieving metadata")
		return nil, err
	}
	s.log.Debug("Metadata retrieved successfully")
	return image, nil
}
