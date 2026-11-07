#include <stddef.h>
#include <sys/types.h>
int cl_listen(void);
int cl_accept(int listener);
int cl_peer(int fd, uid_t *uid, pid_t *pid);
int cl_tun(char *name, size_t capacity);
int cl_send_device(int socket_fd, int device_fd);
int cl_line(int fd, char *buffer, size_t capacity);
int cl_write(int fd, const char *text);
void cl_timeout(int fd, int seconds);
const unsigned char *cl_embedded_info(size_t *length);
