#include "TunnelIPC.h"
#include <sys/socket.h>
#include <sys/un.h>
#include <sys/kern_control.h>
#include <sys/sys_domain.h>
#include <sys/ioctl.h>
#include <sys/stat.h>
#include <net/if_utun.h>
#include <unistd.h>
#include <fcntl.h>
#include <errno.h>
#include <string.h>
#include <stdio.h>
#include <stdlib.h>
#include <launch.h>
#include <poll.h>
#include <mach-o/getsect.h>
extern const struct mach_header_64 _mh_execute_header;

int cl_listen(void) {
    int *fds = NULL; size_t count = 0;
    if (launch_activate_socket("TunnelSocket", &fds, &count) != 0) return -1;
    if (count != 1) { for (size_t i=0;i<count;i++) close(fds[i]);free(fds);return -1; }
    int fd = fds[0]; free(fds);
    fcntl(fd, F_SETFD, FD_CLOEXEC);
    return fd;
}
int cl_accept(int listener) { struct pollfd poller={.fd=listener,.events=POLLIN};int ready=poll(&poller,1,30000);if(ready==0){errno=0;return -2;}if(ready<0)return -1;int fd = accept(listener, NULL, NULL); if (fd >= 0) { fcntl(fd,F_SETFD,FD_CLOEXEC); int yes=1;setsockopt(fd,SOL_SOCKET,SO_NOSIGPIPE,&yes,sizeof(yes)); } return fd; }
int cl_peer(int fd, uid_t *uid, pid_t *pid) {
    gid_t gid;
    socklen_t size = sizeof(*pid);
    return getpeereid(fd, uid, &gid) == 0 && getsockopt(fd, SOL_LOCAL, LOCAL_PEERPID, pid, &size) == 0 ? 0 : -1;
}
int cl_tun(char *name, size_t capacity) {
    int fd = socket(PF_SYSTEM, SOCK_DGRAM, SYSPROTO_CONTROL);
    if (fd < 0) return -1;
    fcntl(fd,F_SETFD,FD_CLOEXEC);
    struct ctl_info info = {0}; strlcpy(info.ctl_name, UTUN_CONTROL_NAME, sizeof(info.ctl_name));
    struct sockaddr_ctl address = {0}; address.sc_len = sizeof(address); address.sc_family = AF_SYSTEM; address.ss_sysaddr = AF_SYS_CONTROL;
    if (ioctl(fd,CTLIOCGINFO,&info) < 0) { close(fd); return -1; }
    address.sc_id = info.ctl_id;
    if (connect(fd,(struct sockaddr *)&address,sizeof(address)) < 0) { close(fd); return -1; }
    socklen_t length = (socklen_t)capacity;
    if (getsockopt(fd,SYSPROTO_CONTROL,UTUN_OPT_IFNAME,name,&length) < 0) { close(fd); return -1; }
    fcntl(fd,F_SETFL,fcntl(fd,F_GETFL,0)|O_NONBLOCK);
    return fd;
}
int cl_send_device(int socket_fd, int device_fd) {
    char payload[] = "{\"event\":\"device\"}\n";
    struct iovec io = {.iov_base = payload, .iov_len = sizeof(payload)-1};
    union { struct cmsghdr alignment; char bytes[CMSG_SPACE(sizeof(int))]; } control = {0};
    struct msghdr message = {.msg_iov=&io,.msg_iovlen=1,.msg_control=control.bytes,.msg_controllen=sizeof(control.bytes)};
    struct cmsghdr *header=CMSG_FIRSTHDR(&message); header->cmsg_level=SOL_SOCKET;header->cmsg_type=SCM_RIGHTS;header->cmsg_len=CMSG_LEN(sizeof(int));memcpy(CMSG_DATA(header),&device_fd,sizeof(device_fd));
    return sendmsg(socket_fd,&message,0)==sizeof(payload)-1 ? 0 : -1;
}
int cl_line(int fd, char *buffer, size_t capacity) {
    size_t length=0;
    while(length+1<capacity) { char ch;ssize_t n=read(fd,&ch,1);if(n<0&&errno==EINTR)continue;if(n<=0)return -1;if(ch=='\n'){buffer[length]=0;return (int)length;}buffer[length++]=ch; }
    return -1;
}
int cl_write(int fd,const char *text) { size_t left=strlen(text);while(left){ssize_t n=write(fd,text,left);if(n<0&&errno==EINTR)continue;if(n<=0)return -1;left-=(size_t)n;text+=n;}return 0; }
void cl_timeout(int fd,int seconds) { struct timeval t={.tv_sec=seconds};setsockopt(fd,SOL_SOCKET,SO_RCVTIMEO,&t,sizeof(t));setsockopt(fd,SOL_SOCKET,SO_SNDTIMEO,&t,sizeof(t)); }
const unsigned char *cl_embedded_info(size_t *length) { unsigned long size=0;const unsigned char *data=getsectiondata(&_mh_execute_header,"__TEXT","__info_plist",&size);*length=(size_t)size;return data; }
